package main

import (
	"os"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/imessage"
)

func (br *IMBridge) UpdateMerges(contacts []*imessage.Contact) {
	br.Log.Infofln("Updating chat merges with %d contacts", len(contacts))

	alreadyHandledGUIDs := make(map[string]struct{}, len(contacts)*2)
	portals := make([]*Portal, 0, 16)
	noPortals := make([]string, 0, 16)
	collect := func(localID string) {
		guid := imessage.Identifier{Service: "iMessage", LocalID: localID}.String()
		if _, alreadyHandled := alreadyHandledGUIDs[guid]; alreadyHandled {
			return
		}
		alreadyHandledGUIDs[guid] = struct{}{}
		portal := br.GetPortalByGUIDIfExists(guid)
		if portal == nil {
			noPortals = append(noPortals, guid)
		} else if portal.GUID == guid {
			portals = append(portals, portal)
		} // else: the ID has already been merged into something else, so ignore it for now
	}

	for _, contact := range contacts {
		portals = portals[:0]
		noPortals = noPortals[:0]

		// Find all the portals from the contact (except ones that have already been merged into another GUID)
		for _, phone := range contact.Phones {
			if !strings.HasPrefix(phone, "+") {
				continue
			}
			collect(phone)
		}
		for _, email := range contact.Emails {
			collect(email)
		}

		// If we found more than one existing portal, merge them into the best one
		if len(portals) > 1 {
			bestPortal := portals[0]
			bestPortalIndex := 0
			for i, portal := range portals {
				alreadyHandledGUIDs[portal.GUID] = struct{}{}
				for _, secondaryGUID := range portal.SecondaryGUIDs {
					alreadyHandledGUIDs[secondaryGUID] = struct{}{}
				}
				if len(portal.SecondaryGUIDs) > len(bestPortal.SecondaryGUIDs) {
					bestPortal = portal
					bestPortalIndex = i
				}
			}
			portals[bestPortalIndex], portals[0] = portals[0], portals[bestPortalIndex]
			bestPortal.Merge(portals[1:])
		}
		// If we found any identifiers without a portal, just mark them as merged in the database.
		if len(noPortals) > 1 || (len(noPortals) == 1 && len(portals) > 0) {
			var targetPortal *Portal
			mergeList := noPortals
			if len(portals) == 0 {
				targetPortal = br.GetPortalByGUID(noPortals[0])
				mergeList = noPortals[1:]
			} else {
				targetPortal = portals[0]
			}
			br.Log.Debugfln("Merging %v (with no portals created) into portal %s", mergeList, targetPortal.GUID)
			br.DB.MergedChat.Set(nil, targetPortal.GUID, mergeList...)
			targetPortal.addSecondaryGUIDs(mergeList)
		}
	}
	br.Log.Infoln("Finished merging with contact list")
}

func (portal *Portal) Merge(others []*Portal) {
	roomIDs := make([]id.RoomID, 0, len(others))
	guids := make([]string, 0, len(others))
	alreadyAdded := map[string]struct{}{}
	for _, other := range others {
		if other == portal {
			continue
		}
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
	portal.addSecondaryGUIDs(guids)
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
		partPortal.addSecondaryGUIDs(guids)
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
