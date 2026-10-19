package main

import (
	"encoding/json"
	"go.mau.fi/mautrix-imessage/imessage"
	"reflect"
	"testing"
)

func TestRoomFeaturesAvailableToAndroidRoomStateRefresh(t *testing.T) {
	features := blueBubblesRoomFeatures(imessage.ConnectorCapabilities{SendTapbacks: true}, true, "any")
	bridgeKey := "fi.mau.imessage://any/any;-;test"
	events := roomFeatureStateEvents(bridgeKey, features)
	if len(events) != 2 {
		t.Fatalf("expected modern and Android-compatible state events, got %d", len(events))
	}
	state := make(map[string]map[string]any)
	for _, evt := range events {
		if evt.Type != roomFeaturesEvent || evt.StateKey == nil {
			t.Fatal("invalid room feature state event")
		}
		state[*evt.StateKey] = evt.Content.Parsed.(map[string]any)
	}
	// Android 4.60.0 uses roomState["com.beeper.room_features"][""].
	android := state[""]
	if android == nil || android["reaction"] != 2 {
		t.Fatal("Android's empty-key lookup did not find reaction support")
	}
	if _, restricted := android["allowed_reactions"]; restricted {
		t.Fatal("Android must receive unrestricted Unicode reactions")
	}
	if !reflect.DeepEqual(android, state[bridgeKey]) {
		t.Fatal("Android and modern clients received different capabilities")
	}
	if len(roomFeatureStateEvents("", features)) != 1 {
		t.Fatal("empty primary state key must not create duplicate events")
	}
}

func TestBlueBubblesRoomFeatures(t *testing.T) {
	caps := imessage.ConnectorCapabilities{SendTapbacks: true, SendCaptions: true, EditMessages: true, UnsendMessages: true}
	unicode := blueBubblesRoomFeatures(caps, true, "iMessage")
	raw, err := json.Marshal(unicode)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["allowed_reactions"]; ok {
		t.Fatal("Unicode reactions must omit the classic allowlist")
	}
	if wire["reaction"] != float64(2) || wire["reaction_count"] != float64(1) || wire["custom_emoji_reactions"] != false {
		t.Fatal("Incorrect Unicode/image-emoji capabilities")
	}
	if wire["file"] == nil || wire["reply"] != float64(2) || wire["edit"] != float64(2) || wire["delete"] != float64(2) {
		t.Fatal("Lost existing media/reply/edit/delete features")
	}
	classic := blueBubblesRoomFeatures(caps, false, "iMessage")
	if len(classic["allowed_reactions"].([]string)) != 6 || classic["id"] == unicode["id"] {
		t.Fatal("Classic mode must retain six reactions and a distinct capability ID")
	}
	unified := blueBubblesRoomFeatures(caps, true, "any")
	if unified["reaction"] != 2 || unified["reply"] != 2 {
		t.Fatal("Unified macOS chats must retain connector reaction/reply support")
	}
	if _, restricted := unified["allowed_reactions"]; restricted {
		t.Fatal("Unified macOS chats must allow arbitrary Unicode reactions")
	}
	sms := blueBubblesRoomFeatures(caps, false, "SMS")
	if sms["reaction"] != 2 || sms["reaction_count"] != 1 || len(sms["allowed_reactions"].([]string)) != 6 {
		t.Fatal("SMS relay must expose classic reactions when the private API supports Tapbacks")
	}
	if sms["reply"] != -2 || sms["edit"] != -2 || sms["delete"] != -2 {
		t.Fatal("SMS must not inherit iMessage-only capabilities")
	}
	caps.SendTapbacks = false
	disabled := blueBubblesRoomFeatures(caps, true, "iMessage")
	if disabled["reaction"] != -2 {
		t.Fatal("Advertised reactions without private API")
	}
	if blueBubblesRoomFeatures(caps, true, "SMS")["reaction"] != -2 {
		t.Fatal("SMS must not advertise reactions without private API support")
	}
}

func TestRoomFeatureIDChangesWithReceiptAndTypingCapabilities(t *testing.T) {
	caps := imessage.ConnectorCapabilities{SendTapbacks: true}
	first := blueBubblesRoomFeatures(caps, true, "any")
	caps.SendReadReceipts = true
	receipts := blueBubblesRoomFeatures(caps, true, "any")
	caps.SendTypingNotifications = true
	typing := blueBubblesRoomFeatures(caps, true, "any")
	if first["id"] == receipts["id"] || receipts["id"] == typing["id"] {
		t.Fatal("feature cache ID missed a capability change")
	}
}
