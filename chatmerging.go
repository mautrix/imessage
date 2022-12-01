package main

import (
	"os"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func (portal *Portal) Merge(others []*Portal) {
	roomIDs := make([]id.RoomID, 0, len(others))
	guids := make([]string, 0, len(others))
	alreadyAdded := map[string]struct{}{}
	for _, other := range others {
		if _, ok := alreadyAdded[other.GUID]; ok {
			continue
		}
		if other.MXID != "" {
			roomIDs = append(roomIDs, other.MXID)
		}
		guids = append(guids, other.GUID)
		alreadyAdded[other.GUID] = struct{}{}
		for _, secondaryGUID := range other.SecondaryGUIDs {
			if _, ok := alreadyAdded[secondaryGUID]; !ok {
				alreadyAdded[secondaryGUID] = struct{}{}
				guids = append(guids, secondaryGUID)
			}
		}
	}
	if portal.MXID != "" {
		roomIDs = append(roomIDs, portal.MXID)
	}
	var newRoomID id.RoomID
	var req *mautrix.ReqBeeperMergeRoom
	portal.log.Debugfln("Merging room with %v (%v)", guids, roomIDs)
	if len(roomIDs) > 1 && portal.bridge.Config.Homeserver.Software == bridgeconfig.SoftwareHungry {
		req = &mautrix.ReqBeeperMergeRoom{
			NewRoom: *portal.getRoomCreateContent(),
			Key:     bridgeInfoHandle,
			Rooms:   roomIDs,
			User:    portal.MainIntent().UserID,
		}
		resp, err := portal.MainIntent().BeeperMergeRooms(req)
		if err != nil {
			portal.log.Errorfln("Failed to merge room: %v", err)
			return
		}
		portal.log.Debugfln("Got merged room ID %s", resp.RoomID)
		newRoomID = resp.RoomID
	} else if len(roomIDs) > 1 {
		portal.log.Debugfln("Deleting old rooms as homeserver doesn't support merging")
		for _, other := range others {
			other.Cleanup(false)
		}
	}
	portal.bridge.portalsLock.Lock()
	defer portal.bridge.portalsLock.Unlock()

	txn, err := portal.bridge.DB.Begin()
	if err != nil {
		portal.log.Errorln("Failed to begin transaction to merge rooms:", err)
		return
	}
	portal.log.Debugln("Updating portal GUIDs in message table")
	portal.bridge.DB.Message.MergePortalGUID(txn, portal.GUID, guids...)
	portal.log.Debugln("Updating merged chat table")
	portal.bridge.DB.MergedChat.Set(txn, portal.GUID, guids...)
	for _, guid := range guids {
		portal.bridge.portalsByGUID[guid] = portal
	}
	portal.SecondaryGUIDs = guids
	if newRoomID != "" {
		portal.log.Debugln("Updating in-memory caches")
		for _, roomID := range roomIDs {
			delete(portal.bridge.portalsByMXID, roomID)
		}
		portal.bridge.portalsByMXID[newRoomID] = portal
		portal.MXID = newRoomID
		portal.InSpace = false
		portal.FirstEventID = ""
		portal.Update(txn)
	}
	err = txn.Commit()
	if err != nil {
		portal.log.Errorln("Failed to commit room merge transaction:", err)
	} else {
		portal.log.Infofln("Finished merging %v -> %s / %v -> %s", guids, portal.GUID, roomIDs, newRoomID)
		if newRoomID != "" {
			portal.addToSpace(portal.bridge.user)
			portal.bridge.user.UpdateDirectChats(map[id.UserID][]id.RoomID{portal.GetDMPuppet().MXID: {portal.MXID}})
			for _, user := range req.NewRoom.Invite {
				portal.bridge.StateStore.SetMembership(portal.MXID, user, event.MembershipJoin)
			}
		}
	}
}

func (portal *Portal) Split(splitParts map[string][]string) {
	br := portal.bridge
	log := portal.log
	reqParts := make([]mautrix.BeeperSplitRoomPart, len(splitParts))
	portals := make(map[string]*Portal, len(splitParts))
	portalReq := make(map[string]*mautrix.BeeperSplitRoomPart, len(splitParts))
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	txn, err := br.DB.Begin()
	if err != nil {
		log.Errorln("Failed to begin transaction to split rooms:", err)
		return
	}
	i := -1
	for primaryGUID, guids := range splitParts {
		guids = append(guids, primaryGUID)
		i++
		if primaryGUID == portal.GUID {
			log.Debugfln("Keeping %v in current portal", guids)
			reqParts[i].UserID = portal.MainIntent().UserID
			reqParts[i].NewRoom = *portal.getRoomCreateContent()
			reqParts[i].Values = guids
			portal.LastSeenHandle = portal.GUID
			portals[portal.GUID] = portal
			portalReq[portal.GUID] = &reqParts[i]
			continue
		}
		log.Debugfln("Updating merged chat mapping with %v -> %s", guids, primaryGUID)
		for _, guid := range guids {
			delete(br.portalsByGUID, guid)
		}
		partPortal := br.loadDBPortal(txn, nil, primaryGUID)
		partPortal.SecondaryGUIDs = guids
		partPortal.LastSeenHandle = primaryGUID
		partPortal.preCreateDMSync(nil)
		portals[partPortal.GUID] = partPortal
		portalReq[partPortal.GUID] = &reqParts[i]
		reqParts[i].UserID = partPortal.MainIntent().UserID
		reqParts[i].NewRoom = *partPortal.getRoomCreateContent()
		reqParts[i].Values = guids
		for _, guid := range guids {
			br.portalsByGUID[guid] = partPortal
			res := br.DB.Message.SplitPortalGUID(txn, guid, portal.GUID, primaryGUID)
			log.Debugfln("Moved %d messages with handle %s in portal %s to portal %s", res, guid, portal.GUID, partPortal.GUID)
		}
		br.DB.MergedChat.Set(txn, primaryGUID, guids...)
	}
	wasSplit := false
	if portal.bridge.Config.Homeserver.Software == bridgeconfig.SoftwareHungry {
		var resp *mautrix.RespBeeperSplitRoom
		resp, err = portal.MainIntent().BeeperSplitRoom(&mautrix.ReqBeeperSplitRoom{
			RoomID: portal.MXID,
			Key:    bridgeInfoHandle,
			Parts:  reqParts,
		})
		if err != nil {
			log.Fatalfln("Failed to split rooms: %v", err)
			os.Exit(60)
		}
		log.Debugfln("Room splitting response: %+v", resp.RoomIDs)
		wasSplit = true
		delete(br.portalsByMXID, portal.MXID)
		for guid, partPortal := range portals {
			roomID, ok := resp.RoomIDs[guid]
			if !ok {
				log.Warnfln("Merge didn't return new room ID for %s", guid)
			} else {
				log.Debugfln("Split got room ID for %s: %s", guid, roomID)
				partPortal.MXID = roomID
				partPortal.FirstEventID = ""
				partPortal.InSpace = false
				partPortal.Update(txn)
				br.portalsByMXID[roomID] = partPortal
			}
		}
	}
	err = txn.Commit()
	if err != nil {
		log.Errorln("Failed to commit room split transaction:", err)
	}
	log.Debugfln("Finished splitting room into %+v", splitParts)
	for guid, partPortal := range portals {
		if partPortal.MXID != "" {
			partPortal.addToSpace(br.user)
			br.user.UpdateDirectChats(map[id.UserID][]id.RoomID{partPortal.GetDMPuppet().MXID: {partPortal.MXID}})
			if wasSplit {
				for _, user := range portalReq[guid].NewRoom.Invite {
					br.StateStore.SetMembership(partPortal.MXID, user, event.MembershipJoin)
				}
			}
		}
	}
}
