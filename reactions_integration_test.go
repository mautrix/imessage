package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-imessage/config"
	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/util/dbutil"
	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
)

type failingRemovalAPI struct {
	imessage.API
	fail   bool
	calls  int
	emoji  string
	part   int
	remove bool
}

func (api *failingRemovalAPI) Capabilities() imessage.ConnectorCapabilities {
	return imessage.ConnectorCapabilities{SendTapbacks: true, MessageStatusCheckpoints: true}
}
func (api *failingRemovalAPI) SupportsEmojiTapbacks() bool { return true }
func (api *failingRemovalAPI) SendEmojiTapback(_, _ string, part int, emoji string, remove bool) (*imessage.SendResponse, error) {
	api.calls++
	api.emoji = emoji
	api.part = part
	api.remove = remove
	if api.fail {
		return nil, errors.New("simulated remote failure")
	}
	return &imessage.SendResponse{GUID: "remote-reaction"}, nil
}
func TestFailedEmojiRemovalRetainsReactionForRetry(t *testing.T) {
	raw, err := dbutil.NewWithDialect(filepath.Join(t.TempDir(), "bridge.db"), "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.RawDB.Close()
	db := database.New(raw, log.Create())
	if err = db.Upgrade(); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO portal (guid,name,mxid) VALUES ('iMessage;+;test','Test','!group:test')`,
		`INSERT INTO message (portal_guid,guid,part,mxid,sender_guid,timestamp) VALUES ('iMessage;+;test','message',2,'$message','sender',1)`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tb := db.Tapback.New()
	tb.PortalGUID = "iMessage;+;test"
	tb.MessageGUID = "message"
	tb.MessagePart = 2
	tb.Type = imessage.TapbackCustom
	tb.CustomEmoji = "👩🏽‍💻"
	tb.MXID = "$reaction"
	tb.Insert(nil)
	api := &failingRemovalAPI{fail: true}
	logger := zerolog.Nop()
	base := &bridgeconfig.BaseConfig{}
	br := &IMBridge{Bridge: bridge.Bridge{Config: *base, ZLog: &logger}, Config: &config.Config{BaseConfig: base}, DB: db, IM: api}
	portal := &Portal{Portal: db.Portal.GetByGUID("iMessage;+;test"), Identifier: imessage.Identifier{IsGroup: true}, bridge: br, log: log.Create()}
	evt := &event.Event{ID: "$redaction", Type: event.EventRedaction, RoomID: "!group:test", Redacts: "$reaction"}
	portal.HandleMatrixRedaction(evt)
	if db.Tapback.GetByMXID("$reaction") == nil {
		t.Fatal("failed remote removal deleted its retry state")
	}
	if api.calls != 1 || api.emoji != "👩🏽‍💻" || api.part != 2 || !api.remove {
		t.Fatal("removal did not preserve the Unicode reaction and target part")
	}
	api.fail = false
	portal.HandleMatrixRedaction(evt)
	if db.Tapback.GetByMXID("$reaction") != nil || api.calls != 2 {
		t.Fatal("successful retry did not remove stored reaction")
	}
}
