package bluebubbles

import (
	"encoding/json"
	"testing"

	"go.mau.fi/mautrix-imessage/imessage"
)

func TestNewMessageWithoutChatDoesNotStopReceive(t *testing.T) {
	bb := &blueBubbles{messageChan: make(chan *imessage.Message, 1)}
	for _, payload := range []string{
		`{"guid":"missing-chat","chats":[]}`,
		`{"guid":"empty-chat","chats":[{"guid":""}]}`,
	} {
		if err := bb.handleNewMessage(json.RawMessage(payload)); err == nil {
			t.Fatal("expected a message without a chat GUID to be rejected")
		}
	}
	if len(bb.messageChan) != 0 {
		t.Fatal("a message without a chat GUID entered the receive queue")
	}

	valid := json.RawMessage(`{"guid":"valid-message","chats":[{"guid":"iMessage;-;example"}]}`)
	if err := bb.handleNewMessage(valid); err != nil {
		t.Fatal(err)
	}
	select {
	case received := <-bb.messageChan:
		if received.GUID != "valid-message" || received.ChatGUID != "iMessage;-;example" {
			t.Fatalf("unexpected received message: %+v", received)
		}
	default:
		t.Fatal("a valid message did not enter the receive queue")
	}
}
