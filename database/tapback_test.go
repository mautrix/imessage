package database

import (
	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/maulogger/v2"
	"path/filepath"
	"testing"
)

func TestCustomTapbackMigrationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	open := func() *Database {
		raw, err := dbutil.NewWithDialect(path, "sqlite3")
		if err != nil {
			t.Fatal(err)
		}
		raw.RawDB.SetMaxOpenConns(1)
		db := New(raw, maulogger.Create())
		if err = db.Upgrade(); err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open()
	// This fixture keeps the production DB completely separate.
	if _, err := db.Exec(`INSERT INTO portal (guid,name) VALUES ('chat','Test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message (portal_guid,guid,part,mxid,sender_guid,timestamp) VALUES ('chat','message',0,'$message','sender',1)`); err != nil {
		t.Fatal(err)
	}
	tb := db.Tapback.New()
	tb.PortalGUID = "chat"
	tb.MessageGUID = "message"
	tb.SenderGUID = "sender"
	tb.Type = imessage.TapbackCustom
	tb.CustomEmoji = "👍🏽"
	tb.MXID = "$reaction"
	tb.GUID = "reaction"
	tb.Insert(nil)
	got := db.Tapback.GetByMXID("$reaction")
	if got == nil || got.CustomEmoji != "👍🏽" {
		t.Fatal("emoji not stored")
	}
	got.CustomEmoji = "👩🏽‍💻"
	got.MXID = "$replacement"
	got.Update()
	db.RawDB.Close()
	db = open()
	defer db.RawDB.Close()
	got = db.Tapback.GetByGUID("chat", "message", 0, "sender")
	if got == nil || got.CustomEmoji != "👩🏽‍💻" || got.MXID != "$replacement" {
		t.Fatalf("lost replacement on restart: %+v", got)
	}
	got.Delete()
	if db.Tapback.GetByTapbackGUID("chat", "reaction") != nil {
		t.Fatal("removal failed")
	}
}

func TestUpgradeExistingClassicReaction(t *testing.T) {
	raw, err := dbutil.NewWithDialect(filepath.Join(t.TempDir(), "legacy.db"), "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.RawDB.Close()
	db := New(raw, maulogger.Create())
	fullTable := db.UpgradeTable
	db.UpgradeTable = fullTable[:21]
	if err = db.Upgrade(); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO portal (guid,name) VALUES ('chat','Test')`,
		`INSERT INTO message (portal_guid,guid,part,mxid,sender_guid,timestamp) VALUES ('chat','message',0,'$message','sender',1)`,
		`INSERT INTO tapback (portal_guid,message_guid,message_part,sender_guid,type,mxid) VALUES ('chat','message',0,'sender',2000,'$classic')`,
	} {
		if _, err = db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	db.UpgradeTable = fullTable
	if err = db.Upgrade(); err != nil {
		t.Fatal(err)
	}
	got := db.Tapback.GetByMXID("$classic")
	if got == nil || got.Type != imessage.TapbackLove || got.CustomEmoji != "" {
		t.Fatalf("migration damaged classic reaction: %+v", got)
	}
}
