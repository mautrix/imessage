package main

import (
	"path/filepath"
	"testing"

	"go.mau.fi/mautrix-imessage/config"
	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/util/dbutil"
	log "maunium.net/go/maulogger/v2"
)

func TestContactMetadataIncludesExistingUnifiedRoomsWithoutCreatingPortals(t *testing.T) {
	raw, err := dbutil.NewWithDialect(filepath.Join(t.TempDir(), "bridge.db"), "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.RawDB.Close()
	db := database.New(raw, log.Create())
	if err := db.Upgrade(); err != nil {
		t.Fatal(err)
	}
	br := &IMBridge{DB: db, Config: &config.Config{}, portalsByGUID: make(map[string]*Portal)}
	const address = "+12025550123"
	for _, service := range []string{"iMessage", "any"} {
		guid := imessage.Identifier{Service: service, LocalID: address}.String()
		br.portalsByGUID[guid] = &Portal{Portal: &database.Portal{GUID: guid}}
	}
	puppet := &Puppet{Puppet: &database.Puppet{ID: address}, bridge: br}
	visited := make(map[string]int)
	puppet.updatePortalMeta(func(p *Portal) { visited[p.GUID]++ })
	if len(visited) != 2 {
		t.Fatalf("updated %d rooms, wanted both legacy and unified rooms", len(visited))
	}
	for _, count := range visited {
		if count != 1 {
			t.Fatal("duplicate metadata update")
		}
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM portal").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("contact update created an unused portal")
	}
}
