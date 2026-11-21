package bluebubbles

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mau.fi/mautrix-imessage/imessage"
)

type recoveryBridgeFixture struct {
	imessage.Bridge
	config imessage.PlatformConfig
}

func (fixture *recoveryBridgeFixture) GetConnectorConfig() *imessage.PlatformConfig {
	return &fixture.config
}

func TestGetHistoryRecoveryMessagesScopesBeforeDownloadingAttachments(t *testing.T) {
	const chat = "iMessage;+;example-chat"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/chat/" + chat + "/message":
			if r.URL.Query().Get("after") != "1000" {
				t.Errorf("unexpected after timestamp: %s", r.URL.Query().Get("after"))
			}
			inChat := []Chat{{GUID: chat}}
			_ = json.NewEncoder(w).Encode(MessageQueryResponse{
				Data: []Message{
					{GUID: "wanted", Text: "example", DateCreated: 2000, Chats: inChat},
					{GUID: "wanted", Text: "duplicate", DateCreated: 2000, Chats: inChat, Attachments: []Attachment{{GUID: "excluded"}}},
					{GUID: "outside-chat", DateCreated: 2000, Chats: []Chat{{GUID: "other"}}, Attachments: []Attachment{{GUID: "excluded"}}},
					{GUID: "outside-date", DateCreated: 500, Chats: inChat, Attachments: []Attachment{{GUID: "excluded"}}},
					{GUID: "unselected", DateCreated: 2000, Chats: inChat, Attachments: []Attachment{{GUID: "excluded"}}},
					{GUID: "unavailable", DateCreated: 2000, Chats: inChat, Attachments: []Attachment{{GUID: "unavailable-file"}}},
					{GUID: "invalid", DateCreated: 2000, Chats: inChat, Attachments: []Attachment{{GUID: "invalid-file"}}},
				},
			})
		case "/api/v1/attachment/unavailable-file":
			_, _ = w.Write([]byte(`{"status":200,"data":{"guid":"unavailable-file","mimeType":"text/plain"}}`))
		case "/api/v1/attachment/unavailable-file/download":
			http.NotFound(w, r)
		case "/api/v1/attachment/invalid-file":
			// Non-JSON metadata forces a download error before the media request.
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	bb := &blueBubbles{bridge: &recoveryBridgeFixture{config: imessage.PlatformConfig{BlueBubblesURL: server.URL}}}
	selected, err := bb.GetHistoryRecoveryMessages(chat, time.UnixMilli(1000), "recovery-fixture", map[string]struct{}{
		"wanted": {}, "outside-chat": {}, "outside-date": {}, "unavailable": {}, "invalid": {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].GUID != "wanted" || selected[0].Text != "example" {
		t.Fatalf("unexpected recovery result: %+v", selected)
	}
}
