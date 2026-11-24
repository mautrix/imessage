package database

import (
	"testing"

	"go.mau.fi/util/dbutil"
	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/imessage"
)

func TestTapbackSchemaAndCustomEmojiRoundTrip(t *testing.T) {
	rawDB, err := dbutil.NewWithDialect("file:tapback_test?mode=memory&cache=shared&_foreign_keys=on", "sqlite3")
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	defer rawDB.Close()

	db := New(rawDB, log.DefaultLogger)
	if err := db.Upgrade(); err != nil {
		t.Fatalf("database upgrade failed: %v", err)
	}

	portal := db.Portal.New()
	portal.GUID = "iMessage;-;+15551234567"
	portal.MXID = id.RoomID("!room:example.com")
	portal.Insert(nil)

	msg := db.Message.New()
	msg.PortalGUID = portal.GUID
	msg.GUID = "msg-1"
	msg.Part = 0
	msg.MXID = id.EventID("$evt-msg-1")
	msg.SenderGUID = "iMessage;-;+15551234567"
	msg.Timestamp = 1700000000000
	msg.Insert(nil)

	tb := db.Tapback.New()
	tb.PortalGUID = portal.GUID
	tb.GUID = "tb-guid-1"
	tb.MessageGUID = msg.GUID
	tb.MessagePart = 0
	tb.SenderGUID = ""
	tb.HandleGUID = portal.GUID
	tb.Type = imessage.TapbackCustomEmoji
	tb.Emoji = "🔥"
	tb.MXID = id.EventID("$evt-tb-1")
	tb.Insert(nil)

	got := db.Tapback.GetByMXID(tb.MXID)
	if got == nil {
		t.Fatal("expected tapback to be found by MXID")
	}
	if got.Type != imessage.TapbackCustomEmoji || got.Emoji != "🔥" || got.GUID != "tb-guid-1" {
		t.Fatalf("unexpected tapback from DB: %+v", got)
	}

	got.Emoji = "🎉"
	got.MXID = id.EventID("$evt-tb-2")
	got.Update()

	updated := db.Tapback.GetByGUID(portal.GUID, msg.GUID, 0, "")
	if updated == nil || updated.Emoji != "🎉" || updated.MXID != "$evt-tb-2" {
		t.Fatalf("unexpected updated tapback: %+v", updated)
	}
}
