package bluebubbles

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-imessage/imessage"
)

type readReceiptTestBridge struct {
	imessage.Bridge
	config imessage.PlatformConfig
	log    zerolog.Logger
}

func (br *readReceiptTestBridge) GetConnectorConfig() *imessage.PlatformConfig {
	return &br.config
}

func (br *readReceiptTestBridge) GetZLog() *zerolog.Logger {
	return &br.log
}

func TestStalledReadReceiptDoesNotDelayBlueBubblesSend(t *testing.T) {
	const chatID = "iMessage;-;synthetic"
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/chat/"+chatID+"/read":
			close(readStarted)
			select {
			case <-releaseRead:
			case <-req.Context().Done():
			}
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/message/text":
			fmt.Fprint(w, `{"status":200,"data":{"guid":"test-message","dateCreated":1000,"handle":{"service":"iMessage"}}}`)
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	defer close(releaseRead)

	connector, err := NewBlueBubblesConnector(&readReceiptTestBridge{
		config: imessage.PlatformConfig{BlueBubblesURL: server.URL},
		log:    zerolog.Nop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	bb := connector.(*blueBubbles)
	bb.usingPrivateAPI = true
	defer bb.Stop()
	go bb.runReadReceipts()

	sendDone := make(chan error, 1)
	go func() {
		// Match synchronous Matrix dispatch by waiting for SendReadReceipt to
		// return before starting the next event's send.
		if err := bb.SendReadReceipt(chatID, "test-read-up-to"); err != nil {
			sendDone <- err
			return
		}
		<-readStarted
		result, err := bb.SendMessage(chatID, "synthetic message", "", 0, nil, nil)
		if err == nil && (result == nil || result.GUID != "test-message") {
			err = errors.New("send response did not contain the expected message ID")
		}
		sendDone <- err
	}()

	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled read receipt blocked a later message send")
	}
}

func TestReadReceiptDeadlineBoundsStalledRequest(t *testing.T) {
	releaseRead := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
		case <-releaseRead:
		}
	}))
	defer server.Close()
	defer close(releaseRead)

	connector, err := NewBlueBubblesConnector(&readReceiptTestBridge{
		config: imessage.PlatformConfig{BlueBubblesURL: server.URL},
		log:    zerolog.Nop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	bb := connector.(*blueBubbles)
	defer bb.Stop()

	done := make(chan error, 1)
	go func() {
		done <- bb.markChatRead(queuedReadReceipt{
			chatID:   "iMessage;-;synthetic",
			deadline: time.Now().Add(100 * time.Millisecond),
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected a context deadline error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stalled read receipt ignored its deadline")
	}
}
