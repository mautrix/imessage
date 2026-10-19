package bluebubbles

import (
	"encoding/json"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-imessage/imessage"
	"net/http"
	"net/http/httptest"
	"testing"
)

type testBridge struct {
	imessage.Bridge
	cfg imessage.PlatformConfig
}

func (b *testBridge) GetConnectorConfig() *imessage.PlatformConfig { return &b.cfg }

func TestParseTapbacks(t *testing.T) {
	cases := []struct {
		kind, emoji, want string
		remove            bool
	}{
		{"love", "", "❤️", false}, {"-like", "", "👍️", true},
		{"emoji", "👍🏽", "👍🏽", false}, {"-emoji", "👩🏽‍💻", "👩🏽‍💻", true},
		{"2006", "🇺🇸", "🇺🇸", false}, {"3006", "1️⃣", "1️⃣", true},
		{"💙", "", "💙", false}, {"-🤣", "", "🤣", true},
	}
	for _, tc := range cases {
		tb, err := parseBBTapback("p:2/message", tc.kind, tc.emoji)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if tb.Emoji() != tc.want || tb.Remove != tc.remove || tb.TargetGUID != "message" || tb.TargetPart != 2 {
			t.Fatalf("%+v: %+v", tc, tb)
		}
	}
	for _, kind := range []string{"emoji", "sticker", "not-like", "garbage", ""} {
		if _, err := parseBBTapback("p:0/message", kind, ""); err == nil {
			t.Fatalf("accepted unsupported %q", kind)
		}
	}
	if _, err := parseBBTapback("p:bad/message", "emoji", "💙"); err == nil {
		t.Fatal("accepted malformed target")
	}
}

func TestSendCustomTapbackHTTP(t *testing.T) {
	var requests []SendReactionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/message/react" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var req SendReactionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":200,"data":{"guid":"sent","handle":{"service":"iMessage"},"dateCreated":1000}}`))
	}))
	defer server.Close()
	bridge := &testBridge{cfg: imessage.PlatformConfig{BlueBubblesURL: server.URL, BlueBubblesCustomEmojiReactions: true}}
	bb := &blueBubbles{bridge: bridge, log: zerolog.Nop(), usingPrivateAPI: true, serverOSVersion: "26.0", serverCustomEmojiReactions: true}
	for _, remove := range []bool{false, true} {
		resp, err := bb.SendEmojiTapback("chat", "message", 2, "👩🏽‍💻", remove)
		if err != nil || resp.GUID != "sent" {
			t.Fatalf("send failed: %v", err)
		}
	}
	if len(requests) != 2 || requests[0].Reaction != "👩🏽‍💻" || requests[1].Reaction != "-👩🏽‍💻" || requests[1].PartIndex != 2 {
		t.Fatalf("incorrect payloads: %+v", requests)
	}
	for _, version := range []string{"13.2.1", "14.7", "15.7", "", "invalid"} {
		bb.serverOSVersion = version
		if _, err := bb.SendEmojiTapback("chat", "message", 0, "💙", false); err == nil {
			t.Fatalf("accepted OS %q", version)
		}
	}
	bb.serverOSVersion = "26.0"
	bb.serverCustomEmojiReactions = false
	if bb.SupportsEmojiTapbacks() {
		t.Fatal("missing confirmed server/helper capability")
	}
	bb.serverCustomEmojiReactions = true
	bridge.cfg.BlueBubblesCustomEmojiReactions = false
	if bb.SupportsEmojiTapbacks() {
		t.Fatal("missing opt-in")
	}
	bridge.cfg.BlueBubblesCustomEmojiReactions = true
	bb.usingPrivateAPI = false
	if bb.SupportsEmojiTapbacks() {
		t.Fatal("missing private API")
	}
	if len(requests) != 2 {
		t.Fatal("unsupported sends reached server")
	}
}

func TestInboundCustomEmojiJSON(t *testing.T) {
	for _, payload := range []string{
		`{"guid":"reaction","chats":[{"guid":"chat"}],"associatedMessageGuid":"p:1/message","associatedMessageType":"emoji","associatedMessageEmoji":"👨‍👩‍👧‍👦"}`,
		`{"guid":"reaction","chats":[{"guid":"chat"}],"associatedMessageGuid":"p:1/message","associatedMessageType":"👨‍👩‍👧‍👦"}`,
	} {
		var wire Message
		if err := json.Unmarshal([]byte(payload), &wire); err != nil {
			t.Fatal(err)
		}
		bb := &blueBubbles{log: zerolog.Nop()}
		msg, err := bb.convertBBMessageToiMessage(wire)
		if err != nil {
			t.Fatal(err)
		}
		if msg.Tapback == nil || msg.Tapback.Emoji() != "👨‍👩‍👧‍👦" || msg.Tapback.TargetPart != 1 {
			t.Fatalf("incorrect conversion: %+v", msg)
		}
	}
}
