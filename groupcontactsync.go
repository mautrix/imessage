package main

import (
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Beeper can retain a room-specific profile after the global ghost profile is
// updated. Reconcile existing group members independently of native chat sync:
// even an old group that BlueBubbles no longer returns needs current contacts.
func (br *IMBridge) syncBlueBubblesGroupProfiles(contacts []*Puppet) {
	if br.Config.IMessage.Platform != "bluebubbles" {
		return
	}
	puppets := make(map[id.UserID]*Puppet)
	for _, puppet := range contacts {
		if puppet != nil {
			puppets[puppet.MXID] = puppet
		}
	}
	rooms, checked, updated, failed := 0, 0, 0, 0
	seen := make(map[id.RoomID]bool)
	for _, portal := range br.GetAllPortals() {
		if portal == nil || !portal.Identifier.IsGroup || portal.MXID == "" || seen[portal.MXID] {
			continue
		}
		seen[portal.MXID] = true
		// Use Client directly: inspecting an inaccessible/left room must never
		// cause EnsureJoined to invite or rejoin anyone.
		members, err := br.Bot.Client.JoinedMembers(portal.MXID)
		if err != nil {
			failed++
			continue
		}
		rooms++
		for userID, member := range members.Joined {
			puppet := puppets[userID]
			if puppet == nil {
				continue
			}
			checked++
			name, avatar := puppet.Displayname, puppet.AvatarURL.String()
			if !groupProfileDiffers(member.DisplayName, member.AvatarURL, name, avatar) {
				continue
			}
			changed, err := reconcileGroupMemberProfile(puppet.Intent.Client, portal.MXID, userID, name, avatar)
			if err != nil {
				failed++
			} else if changed {
				updated++
			}
		}
	}
	br.ZLog.Info().Int("rooms", rooms).Int("checked_profiles", checked).
		Int("updated_profiles", updated).Int("failed_operations", failed).
		Msg("BlueBubbles group contact profile sync complete")
}

func groupProfileDiffers(currentName, currentAvatar, name, avatar string) bool {
	return (name != "" && name != currentName) || (avatar != "" && avatar != currentAvatar)
}

func reconcileGroupMemberProfile(client *mautrix.Client, room id.RoomID, user id.UserID, name, avatar string) (bool, error) {
	var content map[string]any
	if err := client.StateEvent(room, event.StateMember, user.String(), &content); err != nil {
		return false, err
	}
	// Fetch fresh state before writing; never resurrect a departed participant.
	if content["membership"] != "join" {
		return false, nil
	}
	currentName, _ := content["displayname"].(string)
	currentAvatar, _ := content["avatar_url"].(string)
	if !groupProfileDiffers(currentName, currentAvatar, name, avatar) {
		return false, nil
	}
	// Preserve unknown extension fields and existing photos when the source has
	// no image. Only the existing ghost's own joined profile is updated.
	if name != "" {
		content["displayname"] = name
	}
	if avatar != "" {
		content["avatar_url"] = avatar
	}
	if _, err := client.SendStateEvent(room, event.StateMember, user.String(), content); err != nil {
		return false, err
	}
	var actual map[string]any
	if err := client.StateEvent(room, event.StateMember, user.String(), &actual); err != nil {
		return false, err
	}
	if actual["membership"] != "join" || actual["displayname"] != content["displayname"] || actual["avatar_url"] != content["avatar_url"] {
		return false, fmt.Errorf("group contact profile readback did not match")
	}
	return true, nil
}
