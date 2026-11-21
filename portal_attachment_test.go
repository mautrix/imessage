package main

import (
	"testing"

	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-imessage/config"
	"go.mau.fi/mautrix-imessage/imessage"
)

type attachmentNoticeConnector struct{ imessage.API }

func (attachmentNoticeConnector) Capabilities() imessage.ConnectorCapabilities {
	return imessage.ConnectorCapabilities{}
}

func TestUnavailableAttachmentKeepsSeparateText(t *testing.T) {
	cfg := &config.Config{}
	cfg.Bridge.CaptionInMessage = true
	portal := &Portal{
		bridge: &IMBridge{Config: cfg, IM: attachmentNoticeConnector{}},
		log:    log.DefaultLogger,
	}
	parts := portal.convertiMessage(&imessage.Message{
		Text:        "message text",
		Attachments: []*imessage.Attachment{{GUID: "unavailable"}},
	}, nil)
	if len(parts) != 2 {
		t.Fatalf("expected an attachment notice and message text, got %d parts", len(parts))
	}
	if parts[0].Content.MsgType != event.MsgNotice || parts[0].Content.Body != "attachment unavailable" {
		t.Fatalf("expected an attachment notice: %+v", parts[0].Content)
	}
	if parts[1].Content.MsgType != event.MsgText || parts[1].Content.Body != "message text" {
		t.Fatalf("expected original message text: %+v", parts[1].Content)
	}
}
