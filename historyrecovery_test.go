package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/config"
	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
)

func TestHistoryRecoveryPlanRequiresExplicitScope(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, invalid := range []string{
		`{}`,
		`{"since":"2025-01-01T00:00:00Z","chats":[],"message_guids":["example"]}`,
		`{"since":"2025-01-01T00:00:00Z","chats":["example"]}`,
		`{"since":"2027-01-01T00:00:00Z","chats":["example"],"message_guids":["example"]}`,
		`{"since":"2025-01-01T00:00:00Z","chats":["example"],"message_guids":[" "]}`,
	} {
		if _, err := parseHistoryRecoveryPlan([]byte(invalid), now); err == nil {
			t.Errorf("accepted invalid plan %s", invalid)
		}
	}
}

type recoveryFixtureConnector struct {
	imessage.API
	messages []*imessage.Message
	requests int
	success  bool
}

func (*recoveryFixtureConnector) Capabilities() imessage.ConnectorCapabilities {
	return imessage.ConnectorCapabilities{}
}

func (fixture *recoveryFixtureConnector) GetHistoryRecoveryMessages(_ string, _ time.Time, _ string, wanted map[string]struct{}) ([]*imessage.Message, error) {
	fixture.requests++
	var selected []*imessage.Message
	for _, message := range fixture.messages {
		if _, ok := wanted[message.GUID]; ok {
			selected = append(selected, message)
		}
	}
	return selected, nil
}

func (fixture *recoveryFixtureConnector) SendBackfillResult(_ string, _ string, success bool, _ map[string][]id.EventID) {
	fixture.success = success
}

func TestStartupHistoryRecoverySendsOnlyMissingPartsAndTapbacks(t *testing.T) {
	var requestsLock sync.Mutex
	var requests []*mautrix.ReqBeeperBatchSend
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/upload") {
			_, _ = w.Write([]byte(`{"content_uri":"mxc://example.test/test-file"}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/batch_send") {
			t.Errorf("unexpected Matrix request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var req mautrix.ReqBeeperBatchSend
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("invalid batch: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requestsLock.Lock()
		requests = append(requests, &req)
		requestsLock.Unlock()
		eventIDs := make([]id.EventID, 0, len(req.Events))
		for _, evt := range req.Events {
			eventIDs = append(eventIDs, evt.ID)
		}
		_ = json.NewEncoder(w).Encode(mautrix.RespBeeperBatchSend{EventIDs: eventIDs})
	}))
	defer server.Close()

	log := maulogger.Create()
	rawDB, err := dbutil.NewWithDialect(filepath.Join(t.TempDir(), "bridge.db"), "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	db := database.New(rawDB, log)
	if err = db.Upgrade(); err != nil {
		t.Fatal(err)
	}
	client, err := mautrix.NewClient(server.URL, "@bridge:example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	intent := &appservice.IntentAPI{Client: client, UserID: client.UserID}
	zlog := zerolog.Nop()
	connector := &recoveryFixtureConnector{}
	br := &IMBridge{
		Bridge: bridge.Bridge{
			Bot: intent, ZLog: &zlog,
			SpecVersions: mautrix.RespVersions{UnstableFeatures: map[string]bool{mautrix.BeeperFeatureBatchSending.UnstableFlag: true}},
		},
		Config: &config.Config{BaseConfig: &bridgeconfig.BaseConfig{
			Homeserver: bridgeconfig.HomeserverConfig{Software: bridgeconfig.SoftwareHungry},
		}},
		IM: connector, DB: db,
	}
	br.Config.Bridge.Backfill.Enable = true
	chat := db.Portal.New()
	chat.GUID, chat.MXID = "iMessage;+;example-chat", "!chat:example.test"
	chat.Insert(nil)
	portal := &Portal{Portal: chat, bridge: br, log: log, zlog: zlog, Identifier: imessage.ParseIdentifier(chat.GUID)}
	otherChat := db.Portal.New()
	otherChat.GUID, otherChat.MXID = "iMessage;+;other-chat", "!other:example.test"
	otherChat.Insert(nil)
	otherPortal := &Portal{Portal: otherChat}

	// The attachment part is mapped under the original portal. Recovery must preserve
	// its mapping and use the original index of the missing text and reaction target.
	knownPart := db.Message.New()
	knownPart.PortalGUID, knownPart.GUID, knownPart.Part, knownPart.MXID = otherChat.GUID, "multipart", 0, "$known:example.test"
	knownPart.Insert(nil)
	attachmentPath := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(attachmentPath, []byte("attachment fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	srcDate := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	message := &imessage.Message{
		GUID: "multipart", ChatGUID: chat.GUID, Time: srcDate, Text: "missing text", IsRead: true,
		Attachments: []*imessage.Attachment{{PathOnDisk: attachmentPath, FileName: "example.txt", MimeType: "text/plain"}},
	}
	reaction := &imessage.Message{
		GUID: "reaction", ChatGUID: chat.GUID, Time: srcDate.Add(time.Minute), IsRead: true,
		Tapback: &imessage.Tapback{Type: imessage.TapbackLike, TargetGUID: "multipart", TargetPart: 1},
	}
	replacement := *reaction
	replacement.GUID = "another-reaction-for-same-slot"
	connector.messages = []*imessage.Message{message, reaction, &replacement}

	t.Setenv(historyRecoveryPlanEnv, "")
	if br.loadStartupRecoveryPlan() != nil {
		t.Fatal("recovery unexpectedly enabled by default")
	}
	if !portal.sendBackfill("normal", []*imessage.Message{reaction}, true, false, false) {
		t.Fatal("default reaction-only catch-up should succeed without a batch")
	}

	filename := filepath.Join(t.TempDir(), "plan.json")
	data := []byte(`{"since":"2025-01-01T00:00:00Z","chats":["iMessage;+;example-chat"],"message_guids":["multipart","reaction","another-reaction-for-same-slot"]}`)
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(historyRecoveryPlanEnv, filename)
	for i := 0; i < 2; i++ {
		recovery := br.loadStartupRecoveryPlan()
		if recovery == nil {
			t.Fatal("valid recovery plan was not loaded")
		}
		recovery.runForPortal(otherPortal)
		recovery.runForPortal(portal)
		if !connector.success {
			t.Fatal("recovery reported failure")
		}
	}

	requestsLock.Lock()
	defer requestsLock.Unlock()
	if connector.requests != 2 || len(requests) != 2 {
		t.Fatalf("got %d scoped requests and %d outbound batches, want 2 and 2", connector.requests, len(requests))
	}
	expectedTypes := []event.Type{event.EventMessage, event.EventReaction}
	for i, req := range requests {
		if !req.Forward || req.MarkReadBy != "" || len(req.Events) != 1 || req.Events[0].Type.Type != expectedTypes[i].Type {
			t.Fatalf("unexpected recovery batch: %+v", req)
		}
	}
	textPart := db.Message.GetByGUID(chat.GUID, message.GUID, 1)
	if textPart == nil || textPart.MXID != requests[0].Events[0].ID || db.Message.GetByGUID(chat.GUID, message.GUID, 0) != nil {
		t.Fatal("recovered message used the wrong part index or duplicated the existing attachment")
	}
	savedReaction := db.Tapback.GetByTapbackGUID(chat.GUID, reaction.GUID)
	if savedReaction == nil || savedReaction.MessagePart != 1 || savedReaction.MXID != requests[1].Events[0].ID {
		t.Fatal("recovered reaction was not mapped to its original target part")
	}
}
