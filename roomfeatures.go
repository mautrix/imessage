package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"go.mau.fi/mautrix-imessage/imessage"
	"maunium.net/go/mautrix/event"
)

var roomFeaturesEvent = event.Type{Type: "com.beeper.room_features", Class: event.StateEventType}

// This is the client-facing capability event used by current Beeper clients.
// It is distinct from the connector's capabilities and BlueBubbles server info.
func blueBubblesRoomFeatures(caps imessage.ConnectorCapabilities, unicodeEmoji bool, service string) map[string]any {
	// macOS unified chats use "any" as their transport identifier. They must
	// retain connector capabilities. SMS reaction support is supplied by the
	// caller, separately from iMessage-only editing/reply features.
	if service == "SMS" {
		caps.EditMessages = false
		caps.UnsendMessages = false
		caps.SendCaptions = false
		caps.SendReadReceipts = false
		caps.SendTypingNotifications = false
	}
	level := func(supported bool) int {
		if supported {
			return 2
		}
		return -2
	}
	files := make(map[string]any)
	for _, kind := range []string{"m.image", "m.audio", "m.video", "m.file", "org.matrix.msc3245.voice", "fi.mau.gif", "m.sticker"} {
		files[kind] = map[string]any{"mime_types": map[string]int{"*/*": 2}, "caption": level(caps.SendCaptions)}
	}
	features := map[string]any{
		"file":            files,
		"max_text_length": 20000,
		"reply":           level(caps.SendTapbacks && service != "SMS"),
		"edit":            level(caps.EditMessages),
		"delete":          level(caps.UnsendMessages),
		"reaction":        level(caps.SendTapbacks),
		"reaction_count":  1,
		// This field means uploaded/custom image emoji, not arbitrary Unicode emoji.
		"custom_emoji_reactions": false,
		"read_receipts":          caps.SendReadReceipts,
		"typing_notifications":   caps.SendTypingNotifications,
	}
	if !unicodeEmoji || !caps.SendTapbacks {
		features["allowed_reactions"] = []string{"❤️", "👍️", "👎️", "😂", "‼️", "❓"}
	}
	encoded, _ := json.Marshal(features)
	digest := sha256.Sum256(encoded)
	features["id"] = "fi.mau.imessage.bluebubbles.v3." + hex.EncodeToString(digest[:8])
	return features
}

func (portal *Portal) getRoomFeatures() map[string]any {
	if portal.bridge.Config.IMessage.Platform != "bluebubbles" {
		return nil
	}
	service := portal.Identifier.Service
	unicodeEmoji := imessage.SupportsEmojiTapbacks(portal.bridge.IM)
	if service == "SMS" {
		unicodeEmoji = false
	}
	return blueBubblesRoomFeatures(portal.bridge.IM.Capabilities(), unicodeEmoji, service)
}

func roomFeatureStateEvents(bridgeInfoStateKey string, features map[string]any) []*event.Event {
	// Current bridge SDKs use the m.bridge state key. Android 4.60.0's room
	// state refresh looks up only the empty key, so publish the same capabilities
	// under both keys. Neither event changes room identity or message routing.
	keys := []string{bridgeInfoStateKey, ""}
	if bridgeInfoStateKey == "" {
		keys = keys[:1]
	}
	events := make([]*event.Event, 0, len(keys))
	for _, key := range keys {
		stateKey := key
		events = append(events, &event.Event{
			Type: roomFeaturesEvent, StateKey: &stateKey,
			Content: event.Content{Parsed: features},
		})
	}
	return events
}

func (portal *Portal) UpdateRoomFeatures() {
	features := portal.getRoomFeatures()
	if portal.MXID == "" || features == nil {
		return
	}
	featureID := features["id"].(string)
	for _, evt := range roomFeatureStateEvents(portal.getBridgeInfoStateKey(), features) {
		cachePrefix := "bluebubbles_room_features:"
		if *evt.StateKey == "" {
			cachePrefix = "bluebubbles_room_features_android:"
		}
		cacheKey := cachePrefix + string(portal.MXID)
		if portal.bridge.DB.KV.Get(cacheKey) == featureID {
			continue
		}
		_, err := portal.bridge.Bot.SendStateEvent(portal.MXID, evt.Type, *evt.StateKey, evt.Content.Parsed)
		if err != nil {
			portal.log.Warnln("Failed to update Beeper room features:", err)
		} else {
			portal.bridge.DB.KV.Set(cacheKey, featureID)
			portal.log.Debugln("Updated Beeper room features")
		}
	}
}

func (br *IMBridge) refreshRoomFeatures() {
	if br.Config.IMessage.Platform != "bluebubbles" {
		return
	}
	for _, portal := range br.GetAllPortals() {
		portal.UpdateRoomFeatures()
	}
	br.Log.Infoln("Beeper room feature refresh complete")
}
