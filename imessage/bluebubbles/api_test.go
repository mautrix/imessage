package bluebubbles

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/ipc"
)

type fakeBridge struct {
	cfg      *imessage.PlatformConfig
	zlog     zerolog.Logger
	mu       sync.Mutex
	statuses []imessage.BridgeStatus
}

func newFakeBridge(url, password string) *fakeBridge {
	return &fakeBridge{
		cfg: &imessage.PlatformConfig{
			Platform:            "bluebubbles",
			BlueBubblesURL:      url,
			BlueBubblesPassword: password,
		},
		zlog: zerolog.Nop(),
	}
}

func (f *fakeBridge) GetIPC() *ipc.Processor                       { return nil }
func (f *fakeBridge) GetLog() log.Logger                           { return log.DefaultLogger }
func (f *fakeBridge) GetZLog() *zerolog.Logger                     { return &f.zlog }
func (f *fakeBridge) GetConnectorConfig() *imessage.PlatformConfig { return f.cfg }
func (f *fakeBridge) PingServer() (time.Time, time.Time, time.Time) {
	now := time.Now()
	return now, now, now
}
func (f *fakeBridge) SendBridgeStatus(state imessage.BridgeStatus) {
	f.mu.Lock()
	f.statuses = append(f.statuses, state)
	f.mu.Unlock()
}
func (f *fakeBridge) ReIDPortal(oldGUID, newGUID string, mergeExisting bool) bool { return false }
func (f *fakeBridge) GetMessagesSince(chatGUID string, since time.Time) []string  { return nil }
func (f *fakeBridge) SetPushKey(req *imessage.PushKeyRequest)                     {}

func (f *fakeBridge) getStatuses() []imessage.BridgeStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]imessage.BridgeStatus, len(f.statuses))
	copy(out, f.statuses)
	return out
}

func newTestConnector(t *testing.T, serverURL string) (*blueBubbles, *fakeBridge) {
	t.Helper()
	fb := newFakeBridge(serverURL, "secret-pw")
	api, err := NewBlueBubblesConnector(fb)
	if err != nil {
		t.Fatalf("NewBlueBubblesConnector failed: %v", err)
	}
	return api.(*blueBubbles), fb
}

func TestContactConversionAndMatching(t *testing.T) {
	bb, _ := newTestConnector(t, "http://localhost:1234")

	// Nil avatar should NOT panic
	c, err := bb.convertBBContactToiMessageContact(&Contact{
		FirstName:   "Alice",
		LastName:    "Smith",
		DisplayName: "Alice Smith",
		Avatar:      nil,
		ID:          float64(42),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.UserGUID != "42" || len(c.Avatar) != 0 {
		t.Fatalf("unexpected contact: %+v", c)
	}

	// Valid base64 avatar
	rawImg := []byte("fake-png-bytes")
	encoded := base64.StdEncoding.EncodeToString(rawImg)
	c2, err := bb.convertBBContactToiMessageContact(&Contact{
		FirstName: "Bob",
		Avatar:    &encoded,
		ID:        "bob-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(c2.Avatar) != string(rawImg) {
		t.Fatalf("expected decoded avatar, got %q", string(c2.Avatar))
	}

	// Matching: case-insensitive email, phone suffix, db preference, and no false match on empty numeric
	bb.contacts = []Contact{
		{
			FirstName:    "API",
			LastName:     "User",
			SourceType:   "api",
			PhoneNumbers: []PhoneNumber{{Address: "(555) 123-4567"}},
			Emails:       []Email{{Address: "Alice@Example.com"}},
		},
		{
			FirstName:    "DB",
			LastName:     "User",
			SourceType:   "db",
			PhoneNumbers: []PhoneNumber{{Address: "+1 555-123-4567"}},
		},
	}

	matched := bb.matchHandleToContact("+15551234567")
	if matched == nil || matched.FirstName != "DB" {
		t.Fatalf("expected DB contact to win over API contact, got %+v", matched)
	}

	matchedEmail := bb.matchHandleToContact("alice@example.com")
	if matchedEmail == nil || matchedEmail.FirstName != "API" {
		t.Fatalf("expected case-insensitive email match, got %+v", matchedEmail)
	}

	// Non-numeric non-email address should not falsely match phones
	if bogus := bb.matchHandleToContact("not-a-phone"); bogus != nil {
		t.Fatalf("expected nil match for non-numeric non-email, got %+v", bogus)
	}

	// SearchContactList trims whitespace
	found, err := bb.SearchContactList("  alice@example.com  ")
	if err != nil || len(found) != 1 {
		t.Fatalf("expected 1 search result, got %d (err=%v)", len(found), err)
	}
}

func TestConvertBBMessageEdgeCases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/attachment/att-ok":
			_ = json.NewEncoder(w).Encode(AttachmentResponse{
				Status: 200,
				Data:   Attachment{GUID: "att-ok", TransferName: "photo.jpg", MimeType: "image/jpeg"},
			})
		case "/api/v1/attachment/att-ok/download":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("jpegdata"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":404,"message":"not found"}`))
		}
	}))
	defer srv.Close()

	bb, _ := newTestConnector(t, srv.URL)

	// 1. Empty Chats returns error instead of panicking
	if _, err := bb.convertBBMessageToiMessage(Message{GUID: "msg-no-chat"}); err == nil {
		t.Fatal("expected error when message has no chats")
	}

	// 2. Shadowed attachment error does not panic and does not leave nil in Attachments;
	//    ThreadOriginatorPart parses part index; ThreadID uses Chats[0].GroupID (not ThreadOriginatorGUID).
	msg, err := bb.convertBBMessageToiMessage(Message{
		GUID:                 "msg-1",
		DateCreated:          1700000000000,
		Text:                 "hello",
		Chats:                []Chat{{GUID: "iMessage;+;chat123", GroupID: "group-uuid-1"}},
		Handle:               Handle{Address: "+15551234567", Service: "iMessage"},
		ThreadOriginatorGUID: "reply-target-guid",
		ThreadOriginatorPart: "2:0:17",
		Attachments: []Attachment{
			{GUID: "att-missing"},
			{GUID: "att-ok"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0] == nil || msg.Attachments[0].FileName != "photo.jpg" {
		t.Fatalf("expected 1 non-nil attachment, got %+v", msg.Attachments)
	}
	defer os.Remove(msg.Attachments[0].PathOnDisk)

	if msg.ReplyToGUID != "reply-target-guid" || msg.ReplyToPart != 2 {
		t.Fatalf("expected reply part 2, got guid=%q part=%d", msg.ReplyToGUID, msg.ReplyToPart)
	}
	if msg.ThreadID != "group-uuid-1" {
		t.Fatalf("expected ThreadID to be chat GroupID 'group-uuid-1', got %q", msg.ThreadID)
	}

	// 3. Custom emoji tapback (numeric 2006 and string -emoji)
	var customMsg Message
	if err := json.Unmarshal([]byte(`{
		"guid": "tb-1",
		"dateCreated": 1700000001000,
		"chats": [{"guid": "iMessage;-;+15551234567"}],
		"handle": {"address": "+15551234567", "service": "iMessage"},
		"associatedMessageGuid": "p:0/msg-1",
		"associatedMessageType": 2006,
		"associatedMessageEmoji": "🔥"
	}`), &customMsg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	convertedTB, err := bb.convertBBMessageToiMessage(customMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if convertedTB.Tapback == nil || convertedTB.Tapback.Type != imessage.TapbackCustomEmoji || convertedTB.Tapback.Emoji() != "🔥" || convertedTB.Tapback.Remove {
		t.Fatalf("unexpected custom tapback: %+v", convertedTB.Tapback)
	}

	// Non-tapback associatedMessageType (e.g. sticker 1000) should NOT set Tapback
	nonTB, err := bb.convertBBMessageToiMessage(Message{
		GUID:                  "sticker-1",
		DateCreated:           1700000002000,
		Chats:                 []Chat{{GUID: "iMessage;-;+15551234567"}},
		AssociatedMessageGUID: "p:0/msg-1",
		AssociatedMessageType: "1000",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nonTB.Tapback != nil {
		t.Fatalf("expected nil tapback for non-tapback associatedMessageType, got %+v", nonTB.Tapback)
	}
}

func TestSendFileCreateGroupAndCustomEmojiTapback(t *testing.T) {
	var (
		mu                  sync.Mutex
		gotFilename         string
		gotSubject          string
		gotHasSubject       bool
		textMessageCalls    int
		gotCreateGroupReq   CreateChatRequest
		gotReactionRequests []SendReactionRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/message/attachment":
			if err := r.ParseMultipartForm(10 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, fh, err := r.FormFile("attachment")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			gotFilename = fh.Filename
			_, gotHasSubject = r.MultipartForm.Value["subject"]
			gotSubject = r.FormValue("subject")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(SendTextResponse{
				Status: 200,
				Data:   Message{GUID: "att-msg-1", DateCreated: 1700000000000},
			})
		case "/api/v1/message/text":
			mu.Lock()
			textMessageCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(SendTextResponse{
				Status: 200,
				Data:   Message{GUID: "txt-msg-1", DateCreated: 1700000000000},
			})
		case "/api/v1/chat/new":
			var req CreateChatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			gotCreateGroupReq = req
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(CreateChatResponse{
				Status: 200,
				Data:   Chat{GUID: "iMessage;+;chat999", GroupID: "grp-999"},
			})
		case "/api/v1/message/react":
			var req SendReactionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			gotReactionRequests = append(gotReactionRequests, req)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(SendReactionResponse{
				Status: 200,
				Data:   Message{GUID: "react-guid-1", DateCreated: 1700000000000},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	bb, _ := newTestConnector(t, srv.URL)

	tmpFile := filepath.Join(t.TempDir(), "vacation.png")
	if err := os.WriteFile(tmpFile, []byte("png-bytes"), 0600); err != nil {
		t.Fatal(err)
	}

	// Without Private API and empty caption: should preserve real filename and NOT call SendMessage
	bb.usingPrivateAPI = false
	resp, err := bb.SendFile("iMessage;-;+15551234567", "", "vacation.png", tmpFile, "", 0, "image/png", false, nil)
	if err != nil {
		t.Fatalf("SendFile failed: %v", err)
	}
	if resp.ChatGUID != "iMessage;-;+15551234567" || resp.Service != "iMessage" {
		t.Fatalf("unexpected SendResponse: %+v", resp)
	}
	if gotFilename != "vacation.png" {
		t.Fatalf("expected filename vacation.png, got %q", gotFilename)
	}
	if gotHasSubject {
		t.Fatal("did not expect subject field without Private API")
	}
	if textMessageCalls != 0 {
		t.Fatalf("expected 0 extra SendMessage calls for empty caption, got %d", textMessageCalls)
	}

	// With Private API and caption: should send subject
	bb.usingPrivateAPI = true
	_, err = bb.SendFile("iMessage;-;+15551234567", "look at this", "vacation.png", tmpFile, "", 0, "image/png", false, nil)
	if err != nil {
		t.Fatalf("SendFile with private API failed: %v", err)
	}
	if !gotHasSubject || gotSubject != "look at this" {
		t.Fatalf("expected subject 'look at this', got has=%v val=%q", gotHasSubject, gotSubject)
	}

	// CreateGroup requires Private API
	bb.usingPrivateAPI = false
	if _, err := bb.CreateGroup([]string{"iMessage;-;+15551111111", "iMessage;-;+15552222222"}); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported without private API, got %v", err)
	}

	bb.usingPrivateAPI = true
	grp, err := bb.CreateGroup([]string{"iMessage;-;+15551111111", "iMessage;-;alice@example.com"})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if grp.GUID != "iMessage;+;chat999" || grp.ThreadID != "grp-999" {
		t.Fatalf("unexpected CreateGroupResponse: %+v", grp)
	}
	if len(gotCreateGroupReq.Addresses) != 2 || gotCreateGroupReq.Addresses[0] != "+15551111111" || gotCreateGroupReq.Addresses[1] != "alice@example.com" || gotCreateGroupReq.Service != "iMessage" {
		t.Fatalf("unexpected CreateChatRequest: %+v", gotCreateGroupReq)
	}

	// SendCustomEmojiTapback add and remove
	if _, err := bb.SendCustomEmojiTapback("iMessage;-;+15551234567", "msg-1", 0, "🔥", false); err != nil {
		t.Fatalf("SendCustomEmojiTapback add failed: %v", err)
	}
	if _, err := bb.SendCustomEmojiTapback("iMessage;-;+15551234567", "msg-1", 0, "🔥", true); err != nil {
		t.Fatalf("SendCustomEmojiTapback remove failed: %v", err)
	}
	if len(gotReactionRequests) != 2 || gotReactionRequests[0].Reaction != "🔥" || gotReactionRequests[1].Reaction != "-🔥" {
		t.Fatalf("unexpected reaction requests: %+v", gotReactionRequests)
	}
}

func TestHTTPStatusErrorsAndBasePathURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bb/api/v1/") {
			http.Error(w, "bad prefix: "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.URL.Path == "/bb/api/v1/server/info" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal server error"))
			return
		}
		_ = json.NewEncoder(w).Encode(ContactResponse{Status: 200, Data: []Contact{}})
	}))
	defer srv.Close()

	bb, _ := newTestConnector(t, srv.URL+"/bb")
	if err := bb.RefreshContactList(); err != nil {
		t.Fatalf("expected base path '/bb' to be preserved, got error: %v", err)
	}
	var info ServerInfoResponse
	if err := bb.apiGet("/api/v1/server/info", nil, &info); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("expected HTTP 500 error, got %v", err)
	}
}

func TestWebSocketHandshakeEventsAndReconnect(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var connCount atomic.Int32
	var gotPong atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/server/info":
			_ = json.NewEncoder(w).Encode(ServerInfoResponse{Status: 200, Data: ServerInfo{PrivateAPI: true}})
		case "/api/v1/contact":
			_ = json.NewEncoder(w).Encode(ContactResponse{Status: 200, Data: []Contact{}})
		case "/api/v1/chat/iMessage;-;+15551234567":
			_ = json.NewEncoder(w).Encode(ChatResponse{
				Status: 200,
				Data: &Chat{
					GUID:        "iMessage;-;+15551234567",
					LastMessage: &Message{GUID: "last-msg-1"},
				},
			})
		case "/socket.io/":
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close()

			n := connCount.Add(1)
			// Expect initial "40" from client
			_, msg, err := ws.ReadMessage()
			if err != nil || string(msg) != "40" {
				return
			}

			// Send Engine.IO handshake
			_ = ws.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"abc","pingInterval":25000,"pingTimeout":20000}`))
			// Send ping and expect pong "3"
			_ = ws.WriteMessage(websocket.TextMessage, []byte("2"))
			_, pong, err := ws.ReadMessage()
			if err == nil && string(pong) == "3" {
				gotPong.Store(true)
			}

			if n == 1 {
				// Short/malformed 42 frames should not panic
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`42[]`))
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`42["new-message"]`))
				// Chat read status changed (read: false ignored, read: true produces IsFromMe: true)
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`42["chat-read-status-changed",{"chatGuid":"iMessage;-;+15551234567","read":false}]`))
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`42["chat-read-status-changed",{"chatGuid":"iMessage;-;+15551234567","read":true}]`))
				// Message send error -> status channel
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`42["message-send-error",{"guid":"err-msg-1","error":22,"chats":[{"guid":"iMessage;-;+15551234567"}]}]`))
				// Updated message edit dedup: first edit goes to messageChan, second with same dateEdited + dateDelivered goes to messageStatusChan
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`42["updated-message",{"guid":"ed-1","text":"edited","dateCreated":1700000000000,"dateEdited":1700000005000,"chats":[{"guid":"iMessage;-;+15551234567"}]}]`))
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`42["updated-message",{"guid":"ed-1","text":"edited","dateCreated":1700000000000,"dateEdited":1700000005000,"dateDelivered":1700000006000,"chats":[{"guid":"iMessage;-;+15551234567"}]}]`))

				// Give client a moment to process, then force disconnect to test reconnect
				time.Sleep(50 * time.Millisecond)
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`41`))
				return
			}

			// Second connection stays open until closed by Stop()
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	bb, fb := newTestConnector(t, srv.URL)
	bb.reconnectBaseDelay = 20 * time.Millisecond
	bb.reconnectMaxDelay = 50 * time.Millisecond
	bb.contactRefreshInterval = time.Hour

	ready := make(chan struct{})
	if err := bb.Start(func() { close(ready) }); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer bb.Stop()

	<-ready

	// Verify read receipt from chat-read-status-changed
	select {
	case rr := <-bb.ReadReceiptChan():
		if !rr.IsFromMe || rr.ReadUpTo != "last-msg-1" || rr.ChatGUID != "iMessage;-;+15551234567" {
			t.Fatalf("unexpected read receipt: %+v", rr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for read receipt")
	}

	// Verify edit message received once
	select {
	case edited := <-bb.MessageChan():
		if edited.GUID != "ed-1" || !edited.IsEdited {
			t.Fatalf("unexpected edited message: %+v", edited)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for edited message")
	}

	// Verify send error + delivered status on ed-1
	gotFailed := false
	gotDelivered := false
	deadline := time.After(2 * time.Second)
	for !gotFailed || !gotDelivered {
		select {
		case st := <-bb.MessageStatusChan():
			if st.GUID == "err-msg-1" && st.Status == "failed" {
				gotFailed = true
			}
			if st.GUID == "ed-1" && st.Status == "delivered" {
				gotDelivered = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for message statuses (gotFailed=%v, gotDelivered=%v)", gotFailed, gotDelivered)
		}
	}

	// Verify reconnect happened after "41"
	reconnectDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(reconnectDeadline) {
		if connCount.Load() >= 2 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if connCount.Load() < 2 {
		t.Fatalf("expected websocket to reconnect, connCount=%d", connCount.Load())
	}
	if !gotPong.Load() {
		t.Fatal("expected websocket to respond to ping with pong")
	}

	// Verify bridge status transitions were reported
	statuses := fb.getStatuses()
	if len(statuses) < 3 {
		t.Fatalf("expected CONNECTED -> TRANSIENT_DISCONNECT -> CONNECTED statuses, got %+v", statuses)
	}
}
