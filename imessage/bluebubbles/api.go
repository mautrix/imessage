package bluebubbles

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/sahilm/fuzzy"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/imessage"
)

const (
	// Ref: https://github.com/BlueBubblesApp/bluebubbles-server/blob/master/packages/server/src/server/events.ts
	NewMessage            string = "new-message"
	MessageSendError      string = "message-send-error"
	MessageUpdated        string = "updated-message"
	ParticipantRemoved    string = "participant-removed"
	ParticipantAdded      string = "participant-added"
	ParticipantLeft       string = "participant-left"
	GroupIconChanged      string = "group-icon-changed"
	GroupIconRemoved      string = "group-icon-removed"
	ChatReadStatusChanged string = "chat-read-status-changed"
	TypingIndicator       string = "typing-indicator"
	GroupNameChanged      string = "group-name-change"
	IMessageAliasRemoved  string = "imessage-alias-removed"
)

type blueBubbles struct {
	bridge            imessage.Bridge
	log               zerolog.Logger
	ws                *websocket.Conn
	messageChan       chan *imessage.Message
	receiptChan       chan *imessage.ReadReceipt
	typingChan        chan *imessage.TypingNotification
	chatChan          chan *imessage.ChatInfo
	contactChan       chan *imessage.Contact
	messageStatusChan chan *imessage.SendMessageStatus
	backfillTaskChan  chan *imessage.BackfillTask

	contactsLastRefresh time.Time
	contacts            []Contact

	bbRequestLock sync.Mutex

	usingPrivateAPI bool
}

func NewBlueBubblesConnector(bridge imessage.Bridge) (imessage.API, error) {
	return &blueBubbles{
		bridge: bridge,
		log:    bridge.GetZLog().With().Str("component", "bluebubbles").Logger(),

		messageChan:       make(chan *imessage.Message, 256),
		receiptChan:       make(chan *imessage.ReadReceipt, 32),
		typingChan:        make(chan *imessage.TypingNotification, 32),
		chatChan:          make(chan *imessage.ChatInfo, 32),
		contactChan:       make(chan *imessage.Contact, 2048),
		messageStatusChan: make(chan *imessage.SendMessageStatus, 32),
		backfillTaskChan:  make(chan *imessage.BackfillTask, 32),
	}, nil
}

func init() {
	imessage.Implementations["bluebubbles"] = NewBlueBubblesConnector
}
func (bb *blueBubbles) Start(readyCallback func()) error {
	bb.log.Trace().Msg("Start")

	if err := bb.connectAndListen(); err != nil {
		return err
	}

	// Preload some caches
	bb.usingPrivateAPI = bb.isPrivateAPI()
	bb.RefreshContactList()

	// Notify main this API is fully loaded
	readyCallback()

	return nil
}

func (bb *blueBubbles) Stop() {
	bb.log.Trace().Msg("Stop")
	bb.stopListening()
}

func (bb *blueBubbles) connectAndListen() error {
	ws, err := bb.connectToWebSocket()
	if err != nil {
		return err
	}

	bb.ws = ws
	go bb.listenWebSocket()

	return nil
}

func (bb *blueBubbles) connectToWebSocket() (*websocket.Conn, error) {
	ws, _, err := websocket.DefaultDialer.Dial(bb.wsUrl(), nil)
	if err != nil {
		return nil, err
	}
	err = ws.WriteMessage(websocket.TextMessage, []byte("40"))
	if err != nil {
		ws.Close() // Close the connection if write fails
		return nil, err
	}
	return ws, nil
}

func (bb *blueBubbles) listenWebSocket() {
	defer func() {
		if bb.ws != nil {
			bb.ws.Close()
		}
	}()
	for {
		if err := bb.pollMessages(); err != nil {
			bb.log.Error().Err(err).Msg("Error polling messages from WebSocket")
			// Reconnect logic here
			if err := bb.reconnect(); err != nil {
				bb.log.Error().Err(err).Msg("Failed to reconnect to WebSocket")
				return
			}
		}
	}
}

func (bb *blueBubbles) pollMessages() error {
	_, payload, err := bb.ws.ReadMessage()
	if err != nil {
		return err
	}

	if bytes.Equal(payload, []byte("2")) {
		bb.log.Debug().Msg("Received ping from BlueBubbles websocket")
		bb.ws.WriteMessage(websocket.TextMessage, []byte("3"))
		return nil
	}

	if bytes.HasPrefix(payload, []byte("42")) {
		payload = bytes.TrimPrefix(payload, []byte("42"))

		var incomingWebsocketMessage []json.RawMessage
		if err := json.Unmarshal(payload, &incomingWebsocketMessage); err != nil {
			bb.log.Error().Err(err).Msg("Error parsing message from BlueBubbles websocket")
			return err
		}

		var websocketMessageType string
		if err := json.Unmarshal(incomingWebsocketMessage[0], &websocketMessageType); err != nil {
			bb.log.Error().Err(err).Msg("Error parsing message type from BlueBubbles websocket")
			return err
		}

		switch websocketMessageType {
		case NewMessage:
			err = bb.handleNewMessage(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling new message")
			}
		case MessageSendError:
			err = bb.handleMessageSendError(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling message send error")
			}
		case MessageUpdated:
			err = bb.handleMessageUpdated(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling message updated")
			}
		case ParticipantRemoved:
			err = bb.handleParticipantRemoved(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling participant removed")
			}
		case ParticipantAdded:
			err = bb.handleParticipantAdded(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling participant added")
			}
		case ParticipantLeft:
			err = bb.handleParticipantLeft(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling participant left")
			}
		case GroupIconChanged:
			err = bb.handleGroupIconChanged(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling group icon changed")
			}
		case GroupIconRemoved:
			err = bb.handleGroupIconRemoved(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling group icon removed")
			}
		case ChatReadStatusChanged:
			err = bb.handleChatReadStatusChanged(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling chat read status changed")
			}
		case TypingIndicator:
			err = bb.handleTypingIndicator(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling typing indicator")
			}
		case GroupNameChanged:
			err = bb.handleGroupNameChanged(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling group name changed")
			}
		case IMessageAliasRemoved:
			err = bb.handleIMessageAliasRemoved(incomingWebsocketMessage[1])
			if err != nil {
				bb.log.Error().Err(err).Msg("Error handling iMessage alias removed")
			}
		default:
			bb.log.Warn().Any("WebsocketMessageType", incomingWebsocketMessage[0]).Msg("Unknown websocket message type")
		}
	}
	return nil
}

func (bb *blueBubbles) reconnect() error {
	const maxRetryCount = 12
	retryCount := 0
	for {
		bb.log.Info().Msg("Attempting to reconnect to BlueBubbles WebSocket...")
		if retryCount >= maxRetryCount {
			err := errors.New("maximum retry attempts reached")
			bb.log.Error().Err(err).Msg("Maximum retry attempts reached, stopping reconnection attempts to BlueBubbles.")
			return err
		}
		retryCount++
		// Exponential backoff: 2^retryCount * 100ms
		sleepTime := time.Duration(math.Pow(2, float64(retryCount))) * 100 * time.Millisecond
		bb.log.Info().Dur("sleepTime", sleepTime).Msg("Sleeping specified duration before retrying...")
		time.Sleep(sleepTime)
		if err := bb.connectAndListen(); err != nil {
			bb.log.Error().Err(err).Msg("Error reconnecting to WebSocket")
		} else {
			bb.log.Info().Msg("Successfully reconnected to BlueBubbles websocket.")
			return nil
		}
	}
}

func (bb *blueBubbles) stopListening() {
	if bb.ws != nil {
		bb.ws.WriteMessage(websocket.CloseMessage, []byte{})
	}
}

func (bb *blueBubbles) handleNewMessage(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleNewMessage")

	var data Message
	err = json.Unmarshal(rawMessage, &data)
	if err != nil {
		bb.log.Warn().Err(err).RawJSON("rawMessage", rawMessage).Str("event", "handleNewMessage").Msg("Failed to parse event data")
		return err
	}

	message, err := bb.convertBBMessageToiMessage(data)

	if err != nil {
		bb.log.Warn().Err(err).RawJSON("rawMessage", rawMessage).Str("event", "handleNewMessage").Msg("Failed to convert message data")
		return err
	}

	select {
	case bb.messageChan <- message:
	default:
		bb.log.Warn().Msg("Incoming message buffer is full")
	}

	return nil
}

func (bb *blueBubbles) handleMessageSendError(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleMessageSendError")
	return nil // beeper should get the error back during the send response
}

func (bb *blueBubbles) handleMessageUpdated(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleMessageUpdated")

	var data Message
	err = json.Unmarshal(rawMessage, &data)
	if err != nil {
		bb.log.Warn().Err(err).RawJSON("rawMessage", rawMessage).Str("event", "handleMessageUpdated").Msg("Failed to parse event data")
		return err
	}

	message, err := bb.convertBBMessageToiMessage(data)

	if err != nil {
		bb.log.Warn().Err(err).RawJSON("rawMessage", rawMessage).Str("event", "handleMessageUpdated").Msg("Failed to convert message data")
		return err
	}

	select {
	case bb.messageChan <- message:
	default:
		bb.log.Warn().Msg("Incoming message buffer is full")
	}

	return nil
}

func (bb *blueBubbles) handleParticipantRemoved(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleParticipantRemoved")

	return bb.chatRoomUpdate(rawMessage, "handleParticipantAdded")
}

func (bb *blueBubbles) handleParticipantAdded(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleParticipantAdded")

	return bb.chatRoomUpdate(rawMessage, "handleParticipantAdded")
}

func (bb *blueBubbles) handleParticipantLeft(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleParticipantLeft")

	return bb.chatRoomUpdate(rawMessage, "handleParticipantLeft")
}

func (bb *blueBubbles) handleGroupIconChanged(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleGroupIconChanged")
	// BB also fires the new message event where the icon gets updated, NOP here
	return nil
}

func (bb *blueBubbles) handleGroupIconRemoved(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleGroupIconRemoved")
	// BB also fires the new message event where the icon gets removed, NOP here
	return nil
}

func (bb *blueBubbles) handleGroupNameChanged(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleGroupNameChanged")
	// BB also fires the new message event where the name gets updated, NOP here
	return nil
}

func (bb *blueBubbles) chatRoomUpdate(rawMessage json.RawMessage, eventName string) (err error) {

	var data Message
	err = json.Unmarshal(rawMessage, &data)
	if err != nil {
		bb.log.Warn().Err(err).RawJSON("rawMessage", rawMessage).Str("event", eventName).Msg("Failed to parse event data")
		return err
	}

	if len(data.Chats) != 1 {
		bb.log.Error().Interface("rawMessage", rawMessage).Str("event", eventName).Msg("Received chat update event without a chat to update")
	}

	chat, err := bb.convertBBChatToiMessageChat(data.Chats[0])

	if err != nil {
		bb.log.Warn().Err(err).RawJSON("rawMessage", rawMessage).Str("event", eventName).Msg("Failed to convert chat data")
		return err
	}

	select {
	case bb.chatChan <- chat:
	default:
		bb.log.Warn().Msg("Incoming chat buffer is full")
	}

	return nil
}

func (bb *blueBubbles) handleChatReadStatusChanged(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleChatReadStatusChanged")

	var rec MessageReadResponse
	err = json.Unmarshal(data, &rec)
	if err != nil {
		bb.log.Warn().Err(err).Msg("Failed to parse incoming read receipt")
		return nil
	}

	chatInfo, err := bb.getChatInfo(rec.ChatGUID)
	if err != nil {
		bb.log.Warn().Err(err).Str("chatID", rec.ChatGUID).Str("event", "handleChatReadStatusChanged").Msg("Failed to fetch chat info")
		return nil
	}

	if chatInfo.Data.LastMessage == nil {
		bb.log.Warn().Msg("Chat info is missing last message")
		return nil
	}

	lastMessage, err := bb.getMessage(chatInfo.Data.LastMessage.GUID)
	if err != nil {
		bb.log.Warn().Err(err).Msg("Failed to get last message")
		return nil
	}

	var now = time.Now()

	var receipt = imessage.ReadReceipt{
		SenderGUID:     lastMessage.Handle.Address, // TODO: Make sure this is the right field?
		IsFromMe:       false,                      // changing this to false as I believe read receipts will always be from others
		ChatGUID:       rec.ChatGUID,
		ReadUpTo:       chatInfo.Data.LastMessage.GUID,
		ReadAt:         now,
		JSONUnixReadAt: timeToFloat(now),
	}

	select {
	case bb.receiptChan <- &receipt:
	default:
		bb.log.Warn().Msg("Incoming receipt buffer is full")
	}
	return nil
}

func (bb *blueBubbles) handleTypingIndicator(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleTypingIndicator")

	var typingNotification TypingNotification
	err = json.Unmarshal(data, &typingNotification)
	if err != nil {
		return err
	}

	notif := imessage.TypingNotification{
		ChatGUID: typingNotification.GUID,
		Typing:   typingNotification.Display,
	}

	select {
	case bb.typingChan <- &notif:
	default:
		bb.log.Warn().Msg("Incoming typing notification buffer is full")
	}
	return nil
}

func (bb *blueBubbles) handleIMessageAliasRemoved(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleIMessageAliasRemoved")
	return ErrNotImplemented
}

// These functions should all be "get" -ting data FROM bluebubbles

var ErrNotImplemented = errors.New("not implemented")

func (bb *blueBubbles) queryChatMessages(query MessageQueryRequest, allResults []Message, paginate bool) ([]Message, error) {
	bb.log.Info().Interface("query", query).Msg("queryChatMessages")

	var resp MessageQueryResponse
	err := bb.apiPost("/api/v1/message/query", query, &resp)
	if err != nil {
		return nil, err
	}

	allResults = append(allResults, resp.Data...)

	nextPageOffset := resp.Metadata.Offset + resp.Metadata.Limit
	if paginate && (nextPageOffset < resp.Metadata.Total) {
		query.Offset = nextPageOffset
		return bb.queryChatMessages(query, allResults, paginate)
	}

	return allResults, nil
}

func (bb *blueBubbles) GetMessagesSinceDate(chatID string, minDate time.Time, backfillID string) (resp []*imessage.Message, err error) {
	bb.log.Trace().Str("chatID", chatID).Time("minDate", minDate).Str("backfillID", backfillID).Msg("GetMessagesSinceDate")

	after := minDate.UnixNano() / int64(time.Millisecond)
	request := MessageQueryRequest{
		ChatGUID: chatID,
		Limit:    100,
		Offset:   0,
		With: []MessageQueryWith{
			MessageQueryWith(MessageQueryWithChat),
			MessageQueryWith(MessageQueryWithChatParticipants),
			MessageQueryWith(MessageQueryWithAttachment),
			MessageQueryWith(MessageQueryWithHandle),
			MessageQueryWith(MessageQueryWithSMS),
		},
		After: &after,
		Sort:  MessageQuerySortDesc,
	}

	messages, err := bb.queryChatMessages(request, []Message{}, true)
	if err != nil {
		bb.log.Error().Err(err).Interface("request", request).Str("chatID", chatID).Time("minDate", minDate).Str("backfillID", backfillID).Str("search", "GetMessagesSinceDate").Msg("Failed to query chat Messages")
		return nil, err
	}

	for _, messsage := range messages {
		imessage, err := bb.convertBBMessageToiMessage(messsage)
		if err != nil {
			bb.log.Warn().Err(err).Msg("Failed to convert message from BlueBubbles format to Matrix format")
			continue
		}
		resp = append(resp, imessage)
	}

	resp = reverseList(resp)

	return resp, nil
}

func (bb *blueBubbles) GetMessagesBetween(chatID string, minDate, maxDate time.Time) (resp []*imessage.Message, err error) {
	bb.log.Trace().Str("chatID", chatID).Time("minDate", minDate).Time("maxDate", maxDate).Msg("GetMessagesBetween")

	after := minDate.UnixNano() / int64(time.Millisecond)
	before := maxDate.UnixNano() / int64(time.Millisecond)
	request := MessageQueryRequest{
		ChatGUID: chatID,
		Limit:    100,
		Offset:   0,
		With: []MessageQueryWith{
			MessageQueryWith(MessageQueryWithChat),
			MessageQueryWith(MessageQueryWithChatParticipants),
			MessageQueryWith(MessageQueryWithAttachment),
			MessageQueryWith(MessageQueryWithHandle),
			MessageQueryWith(MessageQueryWithSMS),
		},
		After:  &after,
		Before: &before,
		Sort:   MessageQuerySortDesc,
	}

	messages, err := bb.queryChatMessages(request, []Message{}, true)
	if err != nil {
		bb.log.Error().Err(err).Interface("request", request).Str("chatID", chatID).Time("minDate", minDate).Time("maxDate", maxDate).Str("search", "GetMessagesBetween").Msg("Failed to query chat Messages")
		return nil, err
	}

	for _, messsage := range messages {
		imessage, err := bb.convertBBMessageToiMessage(messsage)
		if err != nil {
			bb.log.Warn().Err(err).Msg("Failed to convert message from BlueBubbles format to Matrix format")
			continue
		}
		resp = append(resp, imessage)
	}

	resp = reverseList(resp)

	return resp, nil
}

func (bb *blueBubbles) GetMessagesBeforeWithLimit(chatID string, before time.Time, limit int) (resp []*imessage.Message, err error) {
	bb.log.Trace().Str("chatID", chatID).Time("before", before).Int("limit", limit).Msg("GetMessagesBeforeWithLimit")

	_before := before.UnixNano() / int64(time.Millisecond)
	request := MessageQueryRequest{
		ChatGUID: chatID,
		Limit:    int64(limit),
		Offset:   0,
		With: []MessageQueryWith{
			MessageQueryWith(MessageQueryWithChat),
			MessageQueryWith(MessageQueryWithChatParticipants),
			MessageQueryWith(MessageQueryWithAttachment),
			MessageQueryWith(MessageQueryWithHandle),
			MessageQueryWith(MessageQueryWithSMS),
		},
		Before: &_before,
		Sort:   MessageQuerySortDesc,
	}

	messages, err := bb.queryChatMessages(request, []Message{}, false)
	if err != nil {
		bb.log.Error().Err(err).Interface("request", request).Str("chatID", chatID).Time("before", before).Int("limit", limit).Str("search", "GetMessagesBeforeWithLimit").Msg("Failed to query chat Messages")
		return nil, err
	}

	for _, messsage := range messages {
		imessage, err := bb.convertBBMessageToiMessage(messsage)
		if err != nil {
			bb.log.Warn().Err(err).Msg("Failed to convert message from BlueBubbles format to Matrix format")
			continue
		}
		resp = append(resp, imessage)
	}

	resp = reverseList(resp)

	return resp, nil
}

func (bb *blueBubbles) GetMessagesWithLimit(chatID string, limit int, backfillID string) (resp []*imessage.Message, err error) {
	bb.log.Trace().Str("chatID", chatID).Int("limit", limit).Str("backfillID", backfillID).Msg("GetMessagesWithLimit")

	request := MessageQueryRequest{
		ChatGUID: chatID,
		Limit:    int64(limit),
		Offset:   0,
		With: []MessageQueryWith{
			MessageQueryWith(MessageQueryWithChat),
			MessageQueryWith(MessageQueryWithChatParticipants),
			MessageQueryWith(MessageQueryWithAttachment),
			MessageQueryWith(MessageQueryWithHandle),
			MessageQueryWith(MessageQueryWithSMS),
		},
		Sort: MessageQuerySortDesc,
	}

	messages, err := bb.queryChatMessages(request, []Message{}, false)
	if err != nil {
		bb.log.Error().Err(err).Interface("request", request).Str("chatID", chatID).Int("limit", limit).Str("backfillID", backfillID).Str("search", "GetMessagesWithLimit").Msg("Failed to query chat Messages")
		return nil, err
	}

	for _, messsage := range messages {
		imessage, err := bb.convertBBMessageToiMessage(messsage)
		if err != nil {
			bb.log.Warn().Err(err).Msg("Failed to convert message from BlueBubbles format to Matrix format")
			continue
		}
		resp = append(resp, imessage)
	}

	resp = reverseList(resp)

	return resp, nil
}

func (bb *blueBubbles) getMessage(guid string) (*Message, error) {
	bb.log.Trace().Str("guid", guid).Msg("getMessage")

	var messageResponse MessageResponse

	err := bb.apiGet(fmt.Sprintf("/api/v1/message/%s", guid), map[string]string{
		"with": "chats",
	}, &messageResponse)
	if err != nil {
		return nil, err
	}

	return &messageResponse.Data, nil
}

func (bb *blueBubbles) GetMessage(guid string) (resp *imessage.Message, err error) {
	bb.log.Trace().Str("guid", guid).Msg("GetMessage")

	message, err := bb.getMessage(guid)

	if err != nil {
		bb.log.Err(err).Str("guid", guid).Any("response", message).Msg("Failed to get a message from BlueBubbles")

		return nil, err
	}

	resp, err = bb.convertBBMessageToiMessage(*message)

	if err != nil {
		bb.log.Err(err).Str("guid", guid).Any("response", message).Msg("Failed to parse a message from BlueBubbles")

		return nil, err
	}

	return resp, nil

}

func (bb *blueBubbles) queryChats(query ChatQueryRequest, allResults []Chat) ([]Chat, error) {
	bb.log.Info().Interface("query", query).Msg("queryChatMessages")

	var resp ChatQueryResponse
	err := bb.apiPost("/api/v1/chat/query", query, &resp)
	if err != nil {
		return nil, err
	}

	allResults = append(allResults, resp.Data...)

	nextPageOffset := resp.Metadata.Offset + resp.Metadata.Limit
	if nextPageOffset < resp.Metadata.Total {
		query.Offset = nextPageOffset
		query.Limit = resp.Metadata.Limit
		return bb.queryChats(query, allResults)
	}

	return allResults, nil
}

func (bb *blueBubbles) GetChatsWithMessagesAfter(minDate time.Time) (resp []imessage.ChatIdentifier, err error) {
	bb.log.Trace().Time("minDate", minDate).Msg("GetChatsWithMessagesAfter")

	request := ChatQueryRequest{
		Limit:  1000,
		Offset: 0,
		With: []ChatQueryWith{
			ChatQueryWithLastMessage,
			ChatQueryWithSMS,
		},
		Sort: QuerySortLastMessage,
	}

	chats, err := bb.queryChats(request, []Chat{})
	if err != nil {
		bb.log.Error().Err(err).Time("minDate", minDate).Str("search", "GetChatsWithMessagesAfter").Msg("Failed to search for chats")
		return nil, err
	}

	for _, chat := range chats {
		if chat.LastMessage == nil {
			continue
		}
		if (chat.LastMessage.DateCreated / 1000) < minDate.Unix() {
			continue
		}
		resp = append(resp, imessage.ChatIdentifier{
			ChatGUID: chat.GUID,
			ThreadID: chat.GroupID,
		})
	}

	return resp, nil
}

func (bb *blueBubbles) matchHandleToContact(address string) *Contact {
	var matchedContact *Contact

	numericAddress := numericOnly(address)

	for _, c := range bb.contacts {
		var contact *Contact

		// extract only the numbers of every phone (removes `-` and `+`)
		var numericPhones []string
		for _, e := range c.PhoneNumbers {
			numericPhones = append(numericPhones, numericOnly(e.Address))
		}

		var emailStrings = convertEmails(c.Emails)

		var phoneStrings = convertPhones(c.PhoneNumbers)

		// check for exact matches for either an email or phone
		if strings.Contains(address, "@") && containsString(emailStrings, address) {
			contact = &Contact{} // Create a new instance
			*contact = c
		} else if containsString(phoneStrings, numericAddress) {
			contact = &Contact{} // Create a new instance
			*contact = c
		}

		for _, p := range numericPhones {
			matchLengths := []int{15, 14, 13, 12, 11, 10, 9, 8, 7}
			if containsInt(matchLengths, len(p)) && strings.HasSuffix(numericAddress, p) {
				contact = &Contact{} // Create a new instance
				*contact = c
			}
		}

		// Contacts with a source type of "db" were imported into BB and preferable
		if contact != nil && contact.SourceType == "db" {
			return contact
		}

		// Contacts with a source type of "api" are stored on the mac and can be used as fallback in case an imported one isn't found
		if contact != nil && matchedContact == nil {
			matchedContact = &Contact{} // Create a new instance
			*matchedContact = *contact
		}
	}

	return matchedContact
}

func (bb *blueBubbles) SearchContactList(input string) ([]*imessage.Contact, error) {
	bb.log.Trace().Str("input", input).Msg("SearchContactList")

	var matchedContacts []*imessage.Contact

	for _, contact := range bb.contacts {

		contactFields := []string{
			strings.ToLower(contact.FirstName + " " + contact.LastName),
			strings.ToLower(contact.DisplayName),
			strings.ToLower(contact.Nickname),
			strings.ToLower(contact.Nickname),
			strings.ToLower(contact.Nickname),
		}

		for _, phoneNumber := range contact.PhoneNumbers {
			contactFields = append(contactFields, phoneNumber.Address)
		}

		for _, email := range contact.Emails {
			contactFields = append(contactFields, email.Address)
		}

		matches := fuzzy.Find(strings.ToLower(input), contactFields)

		if len(matches) > 0 { //&& matches[0].Score >= 0
			imessageContact, _ := bb.convertBBContactToiMessageContact(&contact)
			matchedContacts = append(matchedContacts, imessageContact)
			continue
		}
	}

	return matchedContacts, nil
}

func (bb *blueBubbles) GetContactInfo(identifier string) (resp *imessage.Contact, err error) {
	bb.log.Trace().Str("identifier", identifier).Msg("GetContactInfo")

	contact := bb.matchHandleToContact(identifier)

	bb.log.Trace().Str("identifier", identifier).Interface("contact", contact).Msg("GetContactInfo: found a contact")

	if contact != nil {
		resp, _ = bb.convertBBContactToiMessageContact(contact)
		return resp, nil

	}
	err = errors.New("no contacts found for address")
	bb.log.Err(err).Str("identifier", identifier).Msg("No contacts matched address, aborting contact retrieval")
	return nil, err
}

func (bb *blueBubbles) GetContactList() (resp []*imessage.Contact, err error) {
	bb.log.Trace().Msg("GetContactList")

	for _, contact := range bb.contacts {
		imessageContact, _ := bb.convertBBContactToiMessageContact(&contact)
		resp = append(resp, imessageContact)
	}

	return resp, nil
}

func (bb *blueBubbles) RefreshContactList() error {
	bb.log.Trace().Msg("refreshContactsList")

	var contactResponse ContactResponse

	err := bb.apiGet("/api/v1/contact", map[string]string{
		"extraProperties": "avatar",
	}, &contactResponse)
	if err != nil {
		return err
	}

	// save contacts for later
	bb.contacts = contactResponse.Data
	bb.contactsLastRefresh = time.Now()

	return nil
}

func (bb *blueBubbles) getChatInfo(chatID string) (*ChatResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Msg("getChatInfo")

	var chatResponse ChatResponse

	// DEVNOTE: it doesn't appear we should URL Encode the chatID... 😬
	//          the BlueBubbles API returned 404s, sometimes, with URL encoding
	err := bb.apiGet(fmt.Sprintf("/api/v1/chat/%s", chatID), map[string]string{
		"with": "participants,lastMessage",
	}, &chatResponse)
	if err != nil {
		return nil, err
	}

	if chatResponse.Data == nil {
		return nil, errors.New("chat is missing data payload")
	}

	return &chatResponse, nil
}

func (bb *blueBubbles) GetChatInfo(chatID, threadID string) (*imessage.ChatInfo, error) {
	bb.log.Trace().Str("chatID", chatID).Str("threadID", threadID).Msg("GetChatInfo")

	chatResponse, err := bb.getChatInfo(chatID)
	if err != nil {
		bb.log.Error().Err(err).Str("chatID", chatID).Str("threadID", threadID).Msg("Failed to fetch chat info")
		return nil, err
	}

	chatInfo, err := bb.convertBBChatToiMessageChat(*chatResponse.Data)
	if err != nil {
		bb.log.Error().Err(err).Str("chatID", chatID).Str("threadID", threadID).Msg("Failed to convert chat info")
		return nil, err
	}

	return chatInfo, nil
}

func (bb *blueBubbles) GetGroupAvatar(chatID string) (*imessage.Attachment, error) {
	bb.log.Trace().Str("chatID", chatID).Msg("GetGroupAvatar")

	chatResponse, err := bb.getChatInfo(chatID)
	if err != nil {
		bb.log.Error().Err(err).Str("chatID", chatID).Msg("Failed to fetch chat info")
		return nil, err
	}

	if chatResponse.Data.Properties == nil ||
		len(chatResponse.Data.Properties) < 1 {
		return nil, nil
	}

	properties := chatResponse.Data.Properties[0]

	if properties.GroupPhotoGUID == nil {
		return nil, nil
	}

	attachment, err := bb.downloadAttachment(*properties.GroupPhotoGUID)
	if err != nil {
		bb.log.Error().Err(err).Str("chatID", chatID).Msg("Failed to download group avatar")
		return nil, err
	}

	return attachment, nil
}

// These functions all provide "channels" to allow concurrent processing in the bridge
func (bb *blueBubbles) MessageChan() <-chan *imessage.Message {
	return bb.messageChan
}

func (bb *blueBubbles) ReadReceiptChan() <-chan *imessage.ReadReceipt {
	return bb.receiptChan
}

func (bb *blueBubbles) TypingNotificationChan() <-chan *imessage.TypingNotification {
	return bb.typingChan
}

func (bb *blueBubbles) ChatChan() <-chan *imessage.ChatInfo {
	return bb.chatChan
}

func (bb *blueBubbles) ContactChan() <-chan *imessage.Contact {
	return bb.contactChan
}

func (bb *blueBubbles) MessageStatusChan() <-chan *imessage.SendMessageStatus {
	return bb.messageStatusChan
}

func (bb *blueBubbles) BackfillTaskChan() <-chan *imessage.BackfillTask {
	return bb.backfillTaskChan
}

func (bb *blueBubbles) SendMessage(chatID, text string, replyTo string, replyToPart int, richLink *imessage.RichLink, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("text", text).Str("replyTo", replyTo).Int("replyToPart", replyToPart).Any("richLink", richLink).Interface("metadata", metadata).Msg("SendMessage")

	var method string
	if bb.usingPrivateAPI {
		method = "private-api"
	} else {
		// we have to use apple-script and send a second message
		method = "apple-script"
	}

	request := SendTextRequest{
		ChatGUID:            chatID,
		Method:              method,
		Message:             text,
		TempGUID:            fmt.Sprintf("temp-%s", RandString(8)),
		SelectedMessageGUID: replyTo,
		PartIndex:           replyToPart,
	}

	var res SendTextResponse

	err := bb.apiPost("/api/v1/message/text", request, &res)
	if err != nil {
		bb.log.Error().Any("response", res).Msg("Failure when sending message to BlueBubbles")
		return nil, err
	}

	if res.Status != 200 {
		bb.log.Error().Int64("statusCode", res.Status).Any("response", res).Msg("Failure when sending message to BlueBubbles")

		return nil, errors.New("could not send message")
	}

	return &imessage.SendResponse{
		GUID:    res.Data.GUID,
		Service: res.Data.Handle.Service,
		Time:    time.UnixMilli(res.Data.DateCreated),
	}, nil
}

func (bb *blueBubbles) isPrivateAPI() bool {
	var serverInfo ServerInfoResponse
	err := bb.apiGet("/api/v1/server/info", nil, &serverInfo)
	if err != nil {
		bb.log.Error().Err(err).Msg("Failed to get server info from BlueBubbles")
		return false
	}

	privateAPI := serverInfo.Data.PrivateAPI

	return privateAPI
}

func (bb *blueBubbles) SendFile(chatID, text, filename string, pathOnDisk string, replyTo string, replyToPart int, mimeType string, voiceMemo bool, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("text", text).Str("filename", filename).Str("pathOnDisk", pathOnDisk).Str("replyTo", replyTo).Int("replyToPart", replyToPart).Str("mimeType", mimeType).Bool("voiceMemo", voiceMemo).Interface("metadata", metadata).Msg("SendFile")

	attachment, err := os.ReadFile(pathOnDisk)
	if err != nil {
		return nil, err
	}

	bb.log.Info().Int("attachmentSize", len(attachment)).Msg("Read attachment from disk")

	var method string
	if bb.usingPrivateAPI {
		method = "private-api"
	} else {
		// we have to use apple-script and send a second message
		method = "apple-script"
	}

	formData := map[string]interface{}{
		"chatGuid":            chatID,
		"tempGuid":            fmt.Sprintf("temp-%s", RandString(8)),
		"name":                filename,
		"method":              method,
		"attachment":          attachment,
		"isAudioMessage":      voiceMemo,
		"selectedMessageGuid": replyTo,
		"partIndex":           replyToPart,
	}

	if bb.usingPrivateAPI {
		formData["subject"] = text
	}

	path := "/api/v1/message/attachment"

	var response SendTextResponse
	if err := bb.apiPostAsFormData(path, formData, &response); err != nil {
		return nil, err
	}

	if !bb.usingPrivateAPI {
		bb.SendMessage(chatID, text, replyTo, replyToPart, nil, nil)
	}

	var imessageSendResponse = imessage.SendResponse{
		GUID:    response.Data.GUID,
		Service: response.Data.Handle.Service,
		Time:    time.UnixMilli(response.Data.DateCreated),
	}

	return &imessageSendResponse, nil
}

func (bb *blueBubbles) SendFileCleanup(sendFileDir string) {
	_ = os.RemoveAll(sendFileDir)
}

func (bb *blueBubbles) SendTapback(chatID, targetGUID string, targetPart int, tapback imessage.TapbackType, remove bool) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("targetGUID", targetGUID).Int("targetPart", targetPart).Interface("tapback", tapback).Bool("remove", remove).Msg("SendTapback")

	var tapbackName = tapback.Name()

	if !bb.usingPrivateAPI {
		bb.log.Warn().Str("chatID", chatID).Str("targetGUID", targetGUID).Str("tapbackName", tapbackName).Bool("remove", remove).Msg("The private-api isn't enabled in BlueBubbles, can't send tapback")
		return nil, errors.ErrUnsupported
	}

	if remove {
		tapbackName = "-" + tapbackName
	}

	request := SendReactionRequest{
		ChatGUID:            chatID,
		SelectedMessageGUID: targetGUID,
		PartIndex:           targetPart,
		Reaction:            tapbackName,
	}

	var res SendReactionResponse

	err := bb.apiPost("/api/v1/message/react", request, &res)
	if err != nil {
		return nil, err
	}

	if res.Status != 200 {
		bb.log.Error().Any("response", res).Msg("Failure when sending message to BlueBubbles")

		return nil, errors.New("could not send message")

	}

	return &imessage.SendResponse{
		GUID:    res.Data.GUID,
		Service: res.Data.Handle.Service,
		Time:    time.UnixMilli(res.Data.DateCreated),
	}, nil
}

func (bb *blueBubbles) SendReadReceipt(chatID, readUpTo string) error {
	bb.log.Trace().Str("chatID", chatID).Str("readUpTo", readUpTo).Msg("SendReadReceipt")

	if !bb.usingPrivateAPI {
		bb.log.Warn().Str("chatID", chatID).Msg("The private-api isn't enabled in BlueBubbles, can't send read receipt")
		return errors.ErrUnsupported
	}

	var res ReadReceiptResponse
	err := bb.apiPost(fmt.Sprintf("/api/v1/chat/%s/read", chatID), nil, &res)

	if err != nil {
		return err
	}

	if res.Status != 200 {
		bb.log.Error().Any("response", res).Str("chatID", chatID).Msg("Failure when marking a chat as read")

		return errors.New("could not mark chat as read")
	}

	bb.log.Trace().Str("chatID", chatID).Msg("Marked a chat as Read")

	return nil
}

func (bb *blueBubbles) SendTypingNotification(chatID string, typing bool) error {
	bb.log.Trace().Str("chatID", chatID).Bool("typing", typing).Msg("SendTypingNotification")

	if !bb.usingPrivateAPI {
		bb.log.Warn().Str("chatID", chatID).Bool("typing", typing).Msg("The private-api isn't enabled in BlueBubbles, can't send typing notification")
		return errors.ErrUnsupported
	}

	var res TypingResponse
	var err error

	if typing {
		err = bb.apiPost(fmt.Sprintf("/api/v1/chat/%s/typing", chatID), nil, &res)
	} else {
		err = bb.apiDelete(fmt.Sprintf("/api/v1/chat/%s/typing", chatID), nil, &res)
	}

	if err != nil {
		return err
	}

	if res.Status != 200 {
		bb.log.Error().Any("response", res).Str("chatID", chatID).Bool("typing", typing).Msg("Failure when updating typing status")

		return errors.New("could not update typing status")
	}

	bb.log.Trace().Str("chatID", chatID).Bool("typing", typing).Msg("Update typing status")

	return nil
}

func (bb *blueBubbles) ResolveIdentifier(address string) (string, error) {
	bb.log.Trace().Str("address", address).Msg("ResolveIdentifier")

	var identifierResponse ResolveIdentifierResponse

	var handle = address
	if !strings.Contains(address, "@") {
		handle = "+" + numericOnly(address)
	}

	err := bb.apiGet(fmt.Sprintf("/api/v1/handle/%s", handle), nil, &identifierResponse)
	if err != nil {
		bb.log.Error().Any("response", identifierResponse).Str("address", address).Str("handle", handle).Msg("Failure when Resolving Identifier")
		return "", err
	}

	return identifierResponse.Data.Service + ";-;" + identifierResponse.Data.Address, nil
}

func (bb *blueBubbles) PrepareDM(guid string) error {
	bb.log.Trace().Str("guid", guid).Msg("PrepareDM")
	return nil
}

func (bb *blueBubbles) CreateGroup(users []string) (*imessage.CreateGroupResponse, error) {
	bb.log.Trace().Interface("users", users).Msg("CreateGroup")
	return nil, errors.ErrUnsupported
}

// Helper functions

func (bb *blueBubbles) wsUrl() string {
	u, err := url.Parse(strings.Replace(bb.bridge.GetConnectorConfig().BlueBubblesURL, "http", "ws", 1))
	if err != nil {
		bb.log.Error().Err(err).Msg("Error parsing BlueBubbles URL")
		// TODO error handling for bad config
		return ""
	}

	u.Path = "socket.io/"

	q := u.Query()
	q.Add("guid", bb.bridge.GetConnectorConfig().BlueBubblesPassword)
	q.Add("EIO", "4")
	q.Add("transport", "websocket")
	u.RawQuery = q.Encode()

	url := u.String()

	return url
}

func (bb *blueBubbles) apiURL(path string, queryParams map[string]string) string {
	u, err := url.Parse(bb.bridge.GetConnectorConfig().BlueBubblesURL)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error parsing BlueBubbles URL")
		// TODO error handling for bad config
		return ""
	}

	u.Path = path

	q := u.Query()
	q.Add("password", bb.bridge.GetConnectorConfig().BlueBubblesPassword)

	for key, value := range queryParams {
		q.Add(key, value)
	}

	u.RawQuery = q.Encode()

	url := u.String()

	return url
}

func (bb *blueBubbles) apiGet(path string, queryParams map[string]string, target interface{}) (err error) {
	url := bb.apiURL(path, queryParams)

	bb.bbRequestLock.Lock()
	response, err := http.Get(url)
	bb.bbRequestLock.Unlock()
	if err != nil {
		bb.log.Error().Err(err).Msg("Error making GET request")
		return err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error reading response body")
		return err
	}

	if err := json.Unmarshal(responseBody, target); err != nil {
		bb.log.Error().Err(err).Msg("Error unmarshalling response body")
		return err
	}

	return nil
}

func (bb *blueBubbles) apiPost(path string, payload interface{}, target interface{}) error {
	return bb.apiRequest("POST", path, payload, target)
}

func (bb *blueBubbles) apiDelete(path string, payload interface{}, target interface{}) error {
	return bb.apiRequest("DELETE", path, payload, target)
}

func (bb *blueBubbles) apiRequest(method, path string, payload interface{}, target interface{}) (err error) {
	url := bb.apiURL(path, map[string]string{})

	var payloadJSON []byte
	if payload != nil {
		payloadJSON, err = json.Marshal(payload)
		if err != nil {
			bb.log.Error().Err(err).Msg("Error marshalling payload")
			return err
		}
	}

	req, err := http.NewRequest(method, url, bytes.NewBuffer(payloadJSON))
	if err != nil {
		bb.log.Error().Err(err).Str("method", method).Msg("Error creating request")
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	bb.bbRequestLock.Lock()
	response, err := client.Do(req)
	bb.bbRequestLock.Unlock()
	if err != nil {
		bb.log.Error().Err(err).Str("method", method).Msg("Error making request")
		return err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error reading response body")
		return err
	}

	if err := json.Unmarshal(responseBody, target); err != nil {
		bb.log.Error().Err(err).Msg("Error unmarshalling response body")
		return err
	}

	return nil
}

func (bb *blueBubbles) apiPostAsFormData(path string, formData map[string]interface{}, target interface{}) error {
	url := bb.apiURL(path, map[string]string{})

	// Create a new buffer to store the file content
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	for key, value := range formData {
		switch v := value.(type) {
		case int, bool:
			writer.WriteField(key, fmt.Sprint(v))
		case string:
			writer.WriteField(key, v)
		case []byte:
			part, err := writer.CreateFormFile(key, "file.bin")
			if err != nil {
				bb.log.Error().Err(err).Msg("Error creating form-data field")
				return err
			}
			_, err = part.Write(v)
			if err != nil {
				bb.log.Error().Err(err).Msg("Error writing file to form-data")
				return err
			}
		default:
			return fmt.Errorf("unable to serialze %s (type %T) into form-data", key, v)
		}
	}

	// Close the multipart writer
	writer.Close()

	// Make the HTTP POST request
	bb.bbRequestLock.Lock()
	response, err := http.Post(url, writer.FormDataContentType(), &body)
	bb.bbRequestLock.Unlock()
	if err != nil {
		bb.log.Error().Err(err).Msg("Error making POST request")
		return err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error reading response body")
		return err
	}

	if err := json.Unmarshal(responseBody, target); err != nil {
		bb.log.Error().Err(err).Msg("Error unmarshalling response body")
		return err
	}

	bb.log.Trace()

	return nil
}

func (bb *blueBubbles) convertBBContactToiMessageContact(bbContact *Contact) (*imessage.Contact, error) {
	var convertedID string
	var imageData []byte
	var err error

	switch id := bbContact.ID.(type) {
	case string:
		// id is already a string, use it as is
		convertedID = id
	case float64:
		// id is a float, convert it to a string
		convertedID = strconv.FormatFloat(id, 'f', -1, 64)
	default:
		bb.log.Error().Interface("id", id).Msg("Unknown type for contact ID")
		convertedID = ""
	}

	if *bbContact.Avatar != "" {
		imageData, err = base64.StdEncoding.DecodeString(*bbContact.Avatar)
		if err != nil {
			bb.log.Error().Err(err).Str("DisplayName", bbContact.DisplayName).Msg("Error decoding contact avatar")
		}
	}

	return &imessage.Contact{
		FirstName: bbContact.FirstName,
		LastName:  bbContact.LastName,
		Nickname:  bbContact.DisplayName,
		Phones:    convertPhones(bbContact.PhoneNumbers),
		Emails:    convertEmails(bbContact.Emails),
		UserGUID:  convertedID,
		Avatar:    imageData,
	}, nil
}

func (bb *blueBubbles) convertBBMessageToiMessage(bbMessage Message) (*imessage.Message, error) {

	var message imessage.Message

	// Convert bluebubbles.Message to imessage.Message
	message.GUID = bbMessage.GUID
	message.Time = time.UnixMilli(bbMessage.DateCreated)
	message.Subject = bbMessage.Subject
	message.Text = bbMessage.Text
	message.ChatGUID = bbMessage.Chats[0].GUID

	// bbMessage.Handle seems to always be the other person,
	// so the sender/target depends on whether the message is from you
	if bbMessage.IsFromMe {
		message.JSONTargetGUID = bbMessage.Handle.Address
		message.Target = imessage.Identifier{
			LocalID: bbMessage.Handle.Address,
			Service: bbMessage.Handle.Service,
			IsGroup: false,
		}
	} else {
		message.JSONSenderGUID = bbMessage.Handle.Address
		message.Sender = imessage.Identifier{
			LocalID: bbMessage.Handle.Address,
			Service: bbMessage.Handle.Service,
			IsGroup: false,
		}
	}

	message.Service = bbMessage.Handle.Service
	message.IsFromMe = bbMessage.IsFromMe
	message.IsRead = bbMessage.DateRead != 0
	if message.IsRead {
		message.ReadAt = time.UnixMilli(bbMessage.DateRead)
	}
	message.IsDelivered = bbMessage.DateDelivered != 0
	message.IsSent = true   // assume yes because we made it to this part of the code
	message.IsEmote = false // emojis seem to send either way, and BB doesn't say whether there is one or not
	message.IsAudioMessage = bbMessage.IsAudioMessage

	message.ReplyToGUID = bbMessage.ThreadOriginatorGUID

	// TODO: ReplyToPart from bluebubbles looks like "0:0:17" in one test I did
	// 		I don't know what the value means, or how to parse it
	// num, err := strconv.Atoi(bbMessage.ThreadOriginatorPart)
	// if err != nil {
	// 	bb.log.Err(err).Str("ThreadOriginatorPart", bbMessage.ThreadOriginatorPart).Msg("Unable to convert ThreadOriginatorPart to an int")
	// } else {
	// 	message.ReplyToPart = num
	// }

	// Tapbacks
	if bbMessage.AssociatedMessageGUID != "" &&
		bbMessage.AssociatedMessageType != "" {
		message.Tapback = &imessage.Tapback{
			TargetGUID: bbMessage.AssociatedMessageGUID,
			Type:       imessage.TapbackFromName(bbMessage.AssociatedMessageType),
		}
		message.Tapback.Parse()
	} else {
		message.Tapback = nil
	}

	// Attachments
	message.Attachments = make([]*imessage.Attachment, len(bbMessage.Attachments))
	for i, attachment := range bbMessage.Attachments {
		attachment, err := bb.downloadAttachment(attachment.GUID)
		if err != nil {
			bb.log.Err(err).Str("attachmentGUID", attachment.GUID).Msg("Failed to download attachment")
			continue
		}

		message.Attachments[i] = attachment
	}

	// Group name, member, and avatar changes all come through as messages
	// with a special ItemType to denote it isn't a regular message
	message.ItemType = imessage.ItemType(bbMessage.ItemType)

	// Changes based on the ItemType, but denotes user or icon add vs remove actions
	message.GroupActionType = imessage.GroupActionType(bbMessage.GroupActionType)
	message.NewGroupName = bbMessage.GroupTitle

	// TODO Richlinks
	// message.RichLink =

	message.ThreadID = bbMessage.ThreadOriginatorGUID

	return &message, nil
}

func (bb *blueBubbles) convertBBChatToiMessageChat(bbChat Chat) (*imessage.ChatInfo, error) {
	members := make([]string, len(bbChat.Participants))

	for i, participant := range bbChat.Participants {
		members[i] = participant.Address
	}

	chatInfo := &imessage.ChatInfo{
		JSONChatGUID: bbChat.GUID,
		Identifier:   imessage.ParseIdentifier(bbChat.GUID),
		DisplayName:  bbChat.DisplayName,
		Members:      members,
		ThreadID:     bbChat.GroupID,
	}

	return chatInfo, nil
}

func (bb *blueBubbles) downloadAttachment(guid string) (attachment *imessage.Attachment, err error) {
	bb.log.Trace().Str("guid", guid).Msg("downloadAttachment")

	var attachmentResponse AttachmentResponse
	err = bb.apiGet(fmt.Sprintf("/api/v1/attachment/%s", guid), map[string]string{}, &attachmentResponse)
	if err != nil {
		bb.log.Err(err).Str("guid", guid).Msg("Failed to get attachment from BlueBubbles")
		return nil, err
	}

	url := bb.apiURL(fmt.Sprintf("/api/v1/attachment/%s/download", guid), map[string]string{})

	response, err := http.Get(url)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error making GET request")
		return nil, err
	}
	defer response.Body.Close()

	tempFile, err := os.CreateTemp(os.TempDir(), guid)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error creating temp file")
		return nil, err
	}
	defer tempFile.Close()

	_, err = io.Copy(tempFile, response.Body)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error copying response body to temp file")
		return nil, err
	}

	return &imessage.Attachment{
		GUID:       guid, // had trouble renaming this one (guid)
		PathOnDisk: tempFile.Name(),
		FileName:   attachmentResponse.Data.TransferName,
		MimeType:   attachmentResponse.Data.MimeType,
	}, nil
}

func convertPhones(phoneNumbers []PhoneNumber) []string {
	var phones []string
	for _, phone := range phoneNumbers {
		// Convert the phone number format as needed
		phones = append(phones, phone.Address)
	}
	return phones
}

func convertEmails(emails []Email) []string {
	var emailAddresses []string
	for _, email := range emails {
		// Convert the email address format as needed
		emailAddresses = append(emailAddresses, email.Address)
	}
	return emailAddresses
}

func timeToFloat(time time.Time) float64 {
	if time.IsZero() {
		return 0
	}
	return float64(time.Unix()) + float64(time.Nanosecond())/1e9
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func RandString(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

func reverseList(input []*imessage.Message) []*imessage.Message {
	// Get the length of the slice
	length := len(input)

	// Create a new slice to store the reversed elements
	reversed := make([]*imessage.Message, length)

	// Iterate over the original slice in reverse order
	for i, value := range input {
		reversed[length-i-1] = value
	}

	return reversed
}

func containsString(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

func containsInt(slice []int, num int) bool {
	for _, n := range slice {
		if n == num {
			return true
		}
	}
	return false
}

func numericOnly(s string) string {
	var result strings.Builder
	for _, char := range s {
		if unicode.IsDigit(char) {
			result.WriteRune(char)
		}
	}
	return result.String()
}

// These functions are probably not necessary

func (bb *blueBubbles) SendMessageBridgeResult(chatID, messageID string, eventID id.EventID, success bool) {
}
func (bb *blueBubbles) SendBackfillResult(chatID, backfillID string, success bool, idMap map[string][]id.EventID) {
}
func (bb *blueBubbles) SendChatBridgeResult(guid string, mxid id.RoomID) {
}
func (bb *blueBubbles) NotifyUpcomingMessage(eventID id.EventID) {
}
func (bb *blueBubbles) PreStartupSyncHook() (resp imessage.StartupSyncHookResponse, err error) {
	return imessage.StartupSyncHookResponse{
		SkipSync: false,
	}, nil
}
func (bb *blueBubbles) PostStartupSyncHook() {
}

func (bb *blueBubbles) Capabilities() imessage.ConnectorCapabilities {
	return imessage.ConnectorCapabilities{
		MessageSendResponses:     true,
		SendTapbacks:             true,
		SendReadReceipts:         true,
		SendTypingNotifications:  true,
		SendCaptions:             true,
		BridgeState:              false,
		MessageStatusCheckpoints: false,
		DeliveredStatus:          true,
		ContactChatMerging:       false,
		RichLinks:                false,
		ChatBridgeResult:         false,
	}
}
