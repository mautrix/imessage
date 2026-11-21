package bluebubbles

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-imessage/imessage"
)

type attachmentTestBridge struct {
	imessage.Bridge
	config imessage.PlatformConfig
}

func (b *attachmentTestBridge) GetConnectorConfig() *imessage.PlatformConfig {
	return &b.config
}

func TestConvertMessageWithUnavailableAttachment(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/attachment/unavailable":
			http.Error(w, "attachment temporarily unavailable", http.StatusServiceUnavailable)
		case "/api/v1/attachment/available":
			io.WriteString(w, `{"data":{"transferName":"example.txt","mimeType":"text/plain"}}`)
		case "/api/v1/attachment/available/download":
			io.WriteString(w, "attachment contents")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	bb := &blueBubbles{
		bridge: &attachmentTestBridge{config: imessage.PlatformConfig{BlueBubblesURL: server.URL}},
		log:    zerolog.Nop(),
	}
	message, err := bb.convertBBMessageToiMessage(Message{
		Text:  "message with attachments",
		Chats: []Chat{{GUID: "fixture-chat"}},
		Attachments: []Attachment{
			{GUID: "unavailable", TransferName: "missing.txt"},
			{GUID: "available"},
		},
	})
	if err != nil {
		t.Fatalf("convert message: %v", err)
	}
	if message.Text != "message with attachments" || len(message.Attachments) != 2 {
		t.Fatalf("message text or attachment positions were lost: %+v", message)
	}
	unavailable := message.Attachments[0]
	if unavailable == nil || unavailable.GUID != "unavailable" || unavailable.PathOnDisk != "" {
		t.Fatalf("failed attachment lost its slot or acquired a file: %+v", unavailable)
	}
	available := message.Attachments[1]
	if available == nil || available.GUID != "available" {
		t.Fatalf("successful attachment lost its slot: %+v", available)
	}
	t.Cleanup(func() { _ = os.Remove(available.PathOnDisk) })
	data, err := available.Read()
	if err != nil || string(data) != "attachment contents" {
		t.Fatalf("read successful attachment: %q, %v", data, err)
	}
}
