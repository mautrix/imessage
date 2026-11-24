package bluebubbles

import (
	"bytes"
	"context"
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
	"path/filepath"
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

	defaultHTTPTimeout            = 30 * time.Second
	defaultMediaHTTPTimeout       = 2 * time.Minute
	defaultWSReadTimeout          = 90 * time.Second
	defaultWSWriteTimeout         = 15 * time.Second
	defaultReconnectBaseDelay     = 1 * time.Second
	defaultReconnectMaxDelay      = 60 * time.Second
	defaultContactRefreshInterval = 1 * time.Hour
	maxTrackedEditedMessages      = 10000
)

var (
	_ imessage.API                   = (*blueBubbles)(nil)
	_ imessage.VenturaFeatures       = (*blueBubbles)(nil)
	_ imessage.CustomEmojiTapbackAPI = (*blueBubbles)(nil)
)

type blueBubbles struct {
	bridge            imessage.Bridge
	log               zerolog.Logger
	ws                *websocket.Conn
	wsMu              sync.Mutex
	wsWriteLock       sync.Mutex
	wsReadTimeout     time.Duration
	messageChan       chan *imessage.Message
	receiptChan       chan *imessage.ReadReceipt
	typingChan        chan *imessage.TypingNotification
	chatChan          chan *imessage.ChatInfo
	contactChan       chan *imessage.Contact
	messageStatusChan chan *imessage.SendMessageStatus
	backfillTaskChan  chan *imessage.BackfillTask

	contactsLock           sync.RWMutex
	contactsLastRefresh    time.Time
	contacts               []Contact
	contactRefreshInterval time.Duration

	editedLock sync.Mutex
	lastEdited map[string]int64

	httpClient      *http.Client
	mediaHTTPClient *http.Client
	bbRequestLock   sync.Mutex

	reconnectBaseDelay time.Duration
	reconnectMaxDelay  time.Duration

	ctx    context.Context
	cancel context.CancelFunc

	usingPrivateAPI bool
}

func NewBlueBubblesConnector(bridge imessage.Bridge) (imessage.API, error) {
	ctx, cancel := context.WithCancel(context.Background())
	return &blueBubbles{
		bridge: bridge,
		log:    bridge.GetZLog().With().Str("component", "bluebubbles").Logger(),

		wsReadTimeout: defaultWSReadTimeout,

		messageChan:       make(chan *imessage.Message, 256),
		receiptChan:       make(chan *imessage.ReadReceipt, 32),
		typingChan:        make(chan *imessage.TypingNotification, 32),
		chatChan:          make(chan *imessage.ChatInfo, 32),
		contactChan:       make(chan *imessage.Contact, 2048),
		messageStatusChan: make(chan *imessage.SendMessageStatus, 32),
		backfillTaskChan:  make(chan *imessage.BackfillTask, 32),

		contactRefreshInterval: defaultContactRefreshInterval,
		lastEdited:             make(map[string]int64),

		httpClient:      &http.Client{Timeout: defaultHTTPTimeout},
		mediaHTTPClient: &http.Client{Timeout: defaultMediaHTTPTimeout},

		reconnectBaseDelay: defaultReconnectBaseDelay,
		reconnectMaxDelay:  defaultReconnectMaxDelay,

		ctx:    ctx,
		cancel: cancel,
	}, nil
}

func init() {
	imessage.Implementations["bluebubbles"] = NewBlueBubblesConnector
}

func (bb *blueBubbles) context() context.Context {
	if bb.ctx != nil {
		return bb.ctx
	}
	return context.Background()
}

func (bb *blueBubbles) getHTTPClient(media bool) *http.Client {
	if media && bb.mediaHTTPClient != nil {
		return bb.mediaHTTPClient
	}
	if bb.httpClient != nil {
		return bb.httpClient
	}
	return &http.Client{Timeout: defaultHTTPTimeout}
}

func (bb *blueBubbles) sendBridgeState(stateEvent, errCode, message string) {
	if bb.bridge == nil {
		return
	}
	bb.bridge.SendBridgeStatus(imessage.BridgeStatus{
		StateEvent: stateEvent,
		Error:      errCode,
		Message:    message,
		RemoteID:   bb.bridge.GetConnectorConfig().BlueBubblesURL,
	})
}

func (bb *blueBubbles) Start(readyCallback func()) error {
	bb.log.Trace().Msg("Start")

	if bb.ctx == nil || bb.ctx.Err() != nil {
		bb.ctx, bb.cancel = context.WithCancel(context.Background())
	}

	// Preload some caches
	bb.usingPrivateAPI = bb.isPrivateAPI()
	if err := bb.RefreshContactList(); err != nil {
		bb.log.Warn().Err(err).Msg("Failed to preload contact list from BlueBubbles")
	}

	ws, err := bb.connectToWebSocket()
	if err != nil {
		bb.sendBridgeState("TRANSIENT_DISCONNECT", "bb-connect-failed", err.Error())
		return err
	}
	bb.setWS(ws)
	bb.sendBridgeState("CONNECTED", "", "")

	go bb.wsLoop()
	go bb.contactRefreshLoop()

	// Notify main this API is fully loaded
	readyCallback()

	return nil
}

func (bb *blueBubbles) Stop() {
	bb.log.Trace().Msg("Stop")
	if bb.cancel != nil {
		bb.cancel()
	}
	bb.stopListening()
}

func (bb *blueBubbles) contactRefreshLoop() {
	interval := bb.contactRefreshInterval
	if interval <= 0 {
		interval = defaultContactRefreshInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-bb.context().Done():
			return
		case <-ticker.C:
			if err := bb.RefreshContactList(); err != nil {
				bb.log.Warn().Err(err).Msg("Failed to periodically refresh contacts from BlueBubbles")
			}
		}
	}
}

func (bb *blueBubbles) setWS(ws *websocket.Conn) {
	bb.wsMu.Lock()
	bb.ws = ws
	bb.wsMu.Unlock()
}

func (bb *blueBubbles) getWS() *websocket.Conn {
	bb.wsMu.Lock()
	ws := bb.ws
	bb.wsMu.Unlock()
	return ws
}

func (bb *blueBubbles) writeWSMessage(messageType int, data []byte) error {
	ws := bb.getWS()
	if ws == nil {
		return errors.New("websocket is not connected")
	}
	bb.wsWriteLock.Lock()
	defer bb.wsWriteLock.Unlock()
	_ = ws.SetWriteDeadline(time.Now().Add(defaultWSWriteTimeout))
	return ws.WriteMessage(messageType, data)
}

func (bb *blueBubbles) connectToWebSocket() (*websocket.Conn, error) {
	wsURL, err := bb.wsUrl()
	if err != nil {
		return nil, err
	}
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = defaultHTTPTimeout
	ws, resp, err := dialer.DialContext(bb.context(), wsURL, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	_ = ws.SetWriteDeadline(time.Now().Add(defaultWSWriteTimeout))
	if err := ws.WriteMessage(websocket.TextMessage, []byte("40")); err != nil {
		_ = ws.Close()
		return nil, err
	}
	return ws, nil
}

func (bb *blueBubbles) wsLoop() {
	for {
		err := bb.pollMessages()
		if err == nil {
			continue
		}
		if bb.context().Err() != nil {
			bb.log.Debug().Msg("BlueBubbles WebSocket loop stopped")
			return
		}
		bb.log.Error().Err(err).Msg("Error polling messages from WebSocket")
		bb.stopListening()
		bb.sendBridgeState("TRANSIENT_DISCONNECT", "bb-ws-disconnected", err.Error())

		if !bb.reconnect() {
			return
		}
	}
}

func (bb *blueBubbles) pollMessages() error {
	ws := bb.getWS()
	if ws == nil {
		return errors.New("websocket is not connected")
	}

	readTimeout := bb.wsReadTimeout
	if readTimeout <= 0 {
		readTimeout = defaultWSReadTimeout
	}
	_ = ws.SetReadDeadline(time.Now().Add(readTimeout))

	_, payload, err := ws.ReadMessage()
	if err != nil {
		return err
	}

	// Engine.IO v4 open handshake: 0{"sid":"...","pingInterval":25000,"pingTimeout":20000}
	if bytes.HasPrefix(payload, []byte("0{")) {
		var hs SocketIOHandshake
		if err := json.Unmarshal(payload[1:], &hs); err != nil {
			bb.log.Warn().Err(err).Msg("Failed to parse Socket.IO handshake")
		} else if hs.PingInterval > 0 || hs.PingTimeout > 0 {
			timeout := time.Duration(hs.PingInterval+hs.PingTimeout)*time.Millisecond + 15*time.Second
			if timeout > 0 {
				bb.wsReadTimeout = timeout
			}
			bb.log.Debug().Int64("pingIntervalMs", hs.PingInterval).Int64("pingTimeoutMs", hs.PingTimeout).Msg("Connected to BlueBubbles Socket.IO")
		}
		return nil
	}

	// Engine.IO v4 ping ("2" or "2probe") -> pong ("3" or "3probe")
	if bytes.Equal(payload, []byte("2")) {
		bb.log.Debug().Msg("Received ping from BlueBubbles websocket")
		if err := bb.writeWSMessage(websocket.TextMessage, []byte("3")); err != nil {
			return fmt.Errorf("failed to write pong: %w", err)
		}
		return nil
	}
	if bytes.HasPrefix(payload, []byte("2")) {
		pong := append([]byte("3"), payload[1:]...)
		if err := bb.writeWSMessage(websocket.TextMessage, pong); err != nil {
			return fmt.Errorf("failed to write probe pong: %w", err)
		}
		return nil
	}

	// Socket.IO v4 namespace connect ack ("40...")
	if bytes.HasPrefix(payload, []byte("40")) {
		bb.log.Debug().Msg("BlueBubbles Socket.IO namespace connected")
		return nil
	}

	// Socket.IO v4 namespace disconnect ("41...")
	if bytes.HasPrefix(payload, []byte("41")) {
		return errors.New("BlueBubbles websocket requested disconnect")
	}

	if bytes.HasPrefix(payload, []byte("42")) {
		payload = bytes.TrimPrefix(payload, []byte("42"))

		var incomingWebsocketMessage []json.RawMessage
		if err := json.Unmarshal(payload, &incomingWebsocketMessage); err != nil {
			bb.log.Error().Err(err).Msg("Error parsing message from BlueBubbles websocket")
			return nil
		}
		if len(incomingWebsocketMessage) < 1 {
			bb.log.Warn().Msg("Received empty event array from BlueBubbles websocket")
			return nil
		}

		var websocketMessageType string
		if err := json.Unmarshal(incomingWebsocketMessage[0], &websocketMessageType); err != nil {
			bb.log.Error().Err(err).Msg("Error parsing message type from BlueBubbles websocket")
			return nil
		}
		if len(incomingWebsocketMessage) < 2 {
			bb.log.Warn().Str("event", websocketMessageType).Msg("Received BlueBubbles websocket event without payload")
			return nil
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
			bb.log.Warn().Str("WebsocketMessageType", websocketMessageType).Msg("Unknown websocket message type")
		}
	}
	return nil
}

func (bb *blueBubbles) reconnect() bool {
	baseDelay := bb.reconnectBaseDelay
	if baseDelay <= 0 {
		baseDelay = defaultReconnectBaseDelay
	}
	maxDelay := bb.reconnectMaxDelay
	if maxDelay <= 0 {
		maxDelay = defaultReconnectMaxDelay
	}

	delay := baseDelay
	attempt := 0
	for {
		attempt++
		// Apply ±20% jitter so reconnects don't synchronize
		jitterFactor := 0.8 + rand.Float64()*0.4
		sleepTime := time.Duration(float64(delay) * jitterFactor)
		bb.log.Info().Int("attempt", attempt).Dur("sleepTime", sleepTime).Msg("Sleeping before reconnecting to BlueBubbles WebSocket...")

		timer := time.NewTimer(sleepTime)
		select {
		case <-bb.context().Done():
			timer.Stop()
			return false
		case <-timer.C:
		}

		ws, err := bb.connectToWebSocket()
		if err != nil {
			bb.log.Error().Err(err).Int("attempt", attempt).Msg("Error reconnecting to BlueBubbles WebSocket")
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
			continue
		}

		bb.setWS(ws)
		bb.usingPrivateAPI = bb.isPrivateAPI()
		bb.sendBridgeState("CONNECTED", "", "")
		bb.log.Info().Int("attempt", attempt).Msg("Successfully reconnected to BlueBubbles WebSocket")
		return true
	}
}

func (bb *blueBubbles) stopListening() {
	bb.wsMu.Lock()
	ws := bb.ws
	bb.ws = nil
	bb.wsMu.Unlock()

	if ws != nil {
		bb.wsWriteLock.Lock()
		_ = ws.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		bb.wsWriteLock.Unlock()
		_ = ws.Close()
	}
}

func (bb *blueBubbles) recordEdited(guid string, dateEdited int64) bool {
	if guid == "" || dateEdited == 0 {
		return false
	}
	bb.editedLock.Lock()
	defer bb.editedLock.Unlock()
	if bb.lastEdited == nil {
		bb.lastEdited = make(map[string]int64)
	}
	if prev, ok := bb.lastEdited[guid]; ok && dateEdited <= prev {
		return false
	}
	if len(bb.lastEdited) >= maxTrackedEditedMessages {
		bb.lastEdited = make(map[string]int64)
	}
	bb.lastEdited[guid] = dateEdited
	return true
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

	if data.DateEdited != 0 {
		bb.recordEdited(data.GUID, data.DateEdited)
	}

	select {
	case bb.messageChan <- message:
	default:
		bb.log.Warn().Msg("Incoming message buffer is full")
	}

	if message.IsEdited || message.IsRetracted {
		return nil // the regular message channel should handle edits and unsends updates
	} else if message.IsRead {
		select {
		case bb.receiptChan <- &imessage.ReadReceipt{
			SenderGUID: message.ChatGUID,
			IsFromMe:   !message.IsFromMe,
			ChatGUID:   message.ChatGUID,
			ReadUpTo:   message.GUID,
			ReadAt:     message.ReadAt,
		}:
		default:
			bb.log.Warn().Msg("Incoming message buffer is full")
		}
	} else if message.IsDelivered {
		select {
		case bb.messageStatusChan <- &imessage.SendMessageStatus{
			GUID:     message.GUID,
			ChatGUID: message.ChatGUID,
			Status:   "delivered",
			Service:  imessage.ParseIdentifier(message.ChatGUID).Service,
		}:
		default:
			bb.log.Warn().Msg("Incoming message buffer is full")
		}
	}

	return nil
}

func (bb *blueBubbles) handleMessageSendError(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleMessageSendError")

	var data Message
	if err = json.Unmarshal(rawMessage, &data); err != nil {
		bb.log.Warn().Err(err).RawJSON("rawMessage", rawMessage).Str("event", "handleMessageSendError").Msg("Failed to parse message send error")
		return err
	}

	if data.GUID == "" || len(data.Chats) == 0 {
		return nil
	}

	chatGUID := data.Chats[0].GUID
	service := data.Handle.Service
	if service == "" {
		service = imessage.ParseIdentifier(chatGUID).Service
	}
	errMsg := "BlueBubbles failed to send message"
	if data.Error != 0 {
		errMsg = fmt.Sprintf("BlueBubbles failed to send message (error code %d)", data.Error)
	}

	select {
	case bb.messageStatusChan <- &imessage.SendMessageStatus{
		GUID:       data.GUID,
		ChatGUID:   chatGUID,
		Status:     "failed",
		Service:    service,
		StatusCode: strconv.Itoa(data.Error),
		Message:    errMsg,
	}:
	default:
		bb.log.Warn().Msg("Incoming message status buffer is full")
	}
	return nil
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

	isNewEdit := message.IsEdited && bb.recordEdited(data.GUID, data.DateEdited)
	if isNewEdit || message.IsRetracted {
		select {
		case bb.messageChan <- message:
		default:
			bb.log.Warn().Msg("Incoming message buffer is full")
		}
	} else if message.IsRead {
		select {
		case bb.receiptChan <- &imessage.ReadReceipt{
			SenderGUID: message.ChatGUID,
			IsFromMe:   !message.IsFromMe,
			ChatGUID:   message.ChatGUID,
			ReadUpTo:   message.GUID,
			ReadAt:     message.ReadAt,
		}:
		default:
			bb.log.Warn().Msg("Incoming message buffer is full")
		}
	} else if message.IsDelivered {
		select {
		case bb.messageStatusChan <- &imessage.SendMessageStatus{
			GUID:     message.GUID,
			ChatGUID: message.ChatGUID,
			Status:   "delivered",
			Service:  imessage.ParseIdentifier(message.ChatGUID).Service,
		}:
		default:
			bb.log.Warn().Msg("Incoming message buffer is full")
		}
	}

	return nil
}

func (bb *blueBubbles) handleParticipantRemoved(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleParticipantRemoved")

	return bb.chatRoomUpdate(rawMessage, "handleParticipantRemoved")
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

	if len(data.Chats) == 0 {
		bb.log.Error().RawJSON("rawMessage", rawMessage).Str("event", eventName).Msg("Received chat update event without a chat to update")
		return errors.New("chat update event has no chats")
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

	if !rec.Read || rec.ChatGUID == "" {
		return nil
	}

	chatInfo, err := bb.getChatInfo(rec.ChatGUID)
	if err != nil {
		bb.log.Warn().Err(err).Str("chatID", rec.ChatGUID).Str("event", "handleChatReadStatusChanged").Msg("Failed to fetch chat info")
		return nil
	}

	if chatInfo.Data == nil || chatInfo.Data.LastMessage == nil {
		bb.log.Warn().Msg("Chat info is missing last message")
		return nil
	}

	now := time.Now()

	// chat-read-status-changed fires when the local BlueBubbles user marks a chat read/unread.
	receipt := imessage.ReadReceipt{
		SenderGUID:     rec.ChatGUID,
		IsFromMe:       true,
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
	err := bb.apiGet(fmt.Sprintf("/api/v1/chat/%s/message", query.ChatGUID), bb.messageQueryRequestToMap(&query), &resp)
	if err != nil {
		return nil, err
	}

	allResults = append(allResults, resp.Data...)

	nextPageOffset := resp.Metadata.Offset + resp.Metadata.Limit

	// Determine the limit for the next page
	var nextLimit int
	if query.Max != nil && *query.Max > 0 {
		nextLimit = int(math.Min(float64(*query.Max), 1000))
	} else {
		nextLimit = 1000
	}

	// If there are more messages to fetch and pagination is enabled
	if paginate && (nextPageOffset < resp.Metadata.Total) {
		// If the next page offset exceeds the maximum limit, adjust the query
		if nextLimit > 0 && nextPageOffset+int64(nextLimit) > resp.Metadata.Total {
			nextLimit = int(resp.Metadata.Total - nextPageOffset)
		}

		// Update the query with the new offset and limit
		query.Offset = int(nextPageOffset)
		query.Limit = nextLimit

		// Recursively call the function for the next page
		return bb.queryChatMessages(query, allResults, paginate)
	}

	return allResults, nil
}

func (bb *blueBubbles) messageQueryRequestToMap(req *MessageQueryRequest) map[string]string {
	m := make(map[string]string)

	m["limit"] = fmt.Sprintf("%d", req.Limit)
	m["offset"] = fmt.Sprintf("%d", req.Offset)
	m["sort"] = string(req.Sort)

	if req.Before != nil {
		m["before"] = fmt.Sprintf("%d", *req.Before)
	}

	if req.After != nil {
		m["after"] = fmt.Sprintf("%d", *req.After)
	}

	// Handling slice of MessageQueryWith
	if len(req.With) > 0 {
		with := ""
		for index, withItem := range req.With {
			if index == 0 {
				with = string(withItem)
			} else {
				with = with + "," + string(withItem)
			}
		}
		m["with"] = with
	}

	return m
}

func (bb *blueBubbles) GetMessagesSinceDate(chatID string, minDate time.Time, backfillID string) (resp []*imessage.Message, err error) {
	bb.log.Trace().Str("chatID", chatID).Time("minDate", minDate).Str("backfillID", backfillID).Msg("GetMessagesSinceDate")

	after := minDate.UnixNano() / int64(time.Millisecond)
	request := MessageQueryRequest{
		ChatGUID: chatID,
		Limit:    100,
		Offset:   0,
		With: []MessageQueryWith{
			MessageQueryWith(MessageQueryWithChatParticipants),
			MessageQueryWith(MessageQueryWithAttachment),
			MessageQueryWith(MessageQueryWithHandle),
			MessageQueryWith(MessageQueryWithAttributeBody),
			MessageQueryWith(MessageQueryWithMessageSummary),
			MessageQueryWith(MessageQueryWithPayloadData),
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
			MessageQueryWith(MessageQueryWithChatParticipants),
			MessageQueryWith(MessageQueryWithAttachment),
			MessageQueryWith(MessageQueryWithHandle),
			MessageQueryWith(MessageQueryWithAttributeBody),
			MessageQueryWith(MessageQueryWithMessageSummary),
			MessageQueryWith(MessageQueryWithPayloadData),
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

	queryLimit := limit
	if queryLimit > 1000 {
		queryLimit = 1000
	}

	request := MessageQueryRequest{
		ChatGUID: chatID,
		Limit:    queryLimit,
		Max:      &limit,
		Offset:   0,
		With: []MessageQueryWith{
			MessageQueryWith(MessageQueryWithChatParticipants),
			MessageQueryWith(MessageQueryWithAttachment),
			MessageQueryWith(MessageQueryWithHandle),
			MessageQueryWith(MessageQueryWithAttributeBody),
			MessageQueryWith(MessageQueryWithMessageSummary),
			MessageQueryWith(MessageQueryWithPayloadData),
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

	queryLimit := limit
	if queryLimit > 1000 {
		queryLimit = 1000
	}

	request := MessageQueryRequest{
		ChatGUID: chatID,
		Limit:    queryLimit,
		Max:      &limit,
		Offset:   0,
		With: []MessageQueryWith{
			MessageQueryWith(MessageQueryWithChatParticipants),
			MessageQueryWith(MessageQueryWithAttachment),
			MessageQueryWith(MessageQueryWithHandle),
			MessageQueryWith(MessageQueryWithAttributeBody),
			MessageQueryWith(MessageQueryWithMessageSummary),
			MessageQueryWith(MessageQueryWithPayloadData),
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
	bb.contactsLock.RLock()
	defer bb.contactsLock.RUnlock()

	var matchedContact *Contact
	isEmail := strings.Contains(address, "@")
	numericAddress := numericOnly(address)

	for _, c := range bb.contacts {
		var contact *Contact

		if isEmail {
			for _, e := range c.Emails {
				if strings.EqualFold(strings.TrimSpace(e.Address), strings.TrimSpace(address)) {
					copied := c
					contact = &copied
					break
				}
			}
		} else if numericAddress != "" {
			for _, p := range c.PhoneNumbers {
				numericPhone := numericOnly(p.Address)
				if numericPhone == "" {
					continue
				}
				if numericPhone == numericAddress {
					copied := c
					contact = &copied
					break
				}
				if len(numericPhone) >= 7 && len(numericPhone) <= 15 &&
					(strings.HasSuffix(numericAddress, numericPhone) || strings.HasSuffix(numericPhone, numericAddress)) {
					copied := c
					contact = &copied
					break
				}
			}
		}

		// Contacts with a source type of "db" were imported into BB and preferable
		if contact != nil && contact.SourceType == "db" {
			return contact
		}

		// Contacts with a source type of "api" are stored on the mac and can be used as fallback in case an imported one isn't found
		if contact != nil && matchedContact == nil {
			copied := *contact
			matchedContact = &copied
		}
	}

	return matchedContact
}

func (bb *blueBubbles) SearchContactList(input string) ([]*imessage.Contact, error) {
	bb.log.Trace().Str("input", input).Msg("SearchContactList")

	query := strings.ToLower(strings.TrimSpace(input))
	if query == "" {
		return nil, nil
	}

	bb.contactsLock.RLock()
	defer bb.contactsLock.RUnlock()

	var matchedContacts []*imessage.Contact

	for i := range bb.contacts {
		contact := bb.contacts[i]
		contactFields := []string{
			strings.ToLower(contact.FirstName + " " + contact.LastName),
			strings.ToLower(contact.DisplayName),
			strings.ToLower(contact.Nickname),
		}

		for _, phoneNumber := range contact.PhoneNumbers {
			contactFields = append(contactFields, strings.ToLower(phoneNumber.Address))
		}

		for _, email := range contact.Emails {
			contactFields = append(contactFields, strings.ToLower(email.Address))
		}

		matches := fuzzy.Find(query, contactFields)

		if len(matches) > 0 {
			imessageContact, _ := bb.convertBBContactToiMessageContact(&contact)
			matchedContacts = append(matchedContacts, imessageContact)
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
	bb.log.Debug().Err(err).Str("identifier", identifier).Msg("No contacts matched address, aborting contact retrieval")
	return nil, err
}

func (bb *blueBubbles) GetContactList() (resp []*imessage.Contact, err error) {
	bb.log.Trace().Msg("GetContactList")

	bb.contactsLock.RLock()
	defer bb.contactsLock.RUnlock()

	resp = make([]*imessage.Contact, 0, len(bb.contacts))
	for i := range bb.contacts {
		contact := bb.contacts[i]
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

	bb.contactsLock.Lock()
	bb.contacts = contactResponse.Data
	bb.contactsLastRefresh = time.Now()
	bb.contactsLock.Unlock()

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

func buildSendResponse(chatID string, msg Message) *imessage.SendResponse {
	service := msg.Handle.Service
	if service == "" {
		service = imessage.ParseIdentifier(chatID).Service
	}
	respChatGUID := chatID
	if len(msg.Chats) > 0 && msg.Chats[0].GUID != "" {
		respChatGUID = msg.Chats[0].GUID
	}
	return &imessage.SendResponse{
		GUID:     msg.GUID,
		Service:  service,
		ChatGUID: respChatGUID,
		Time:     time.UnixMilli(msg.DateCreated),
	}
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
		ChatGUID: chatID,
		Method:   method,
		Message:  text,
		TempGUID: fmt.Sprintf("temp-%s", RandString(8)),
	}
	if replyTo != "" {
		request.SelectedMessageGUID = replyTo
		request.PartIndex = replyToPart
	}

	var res SendTextResponse

	err := bb.apiPost("/api/v1/message/text", request, &res)
	if err != nil {
		bb.log.Error().Err(err).Any("response", res).Msg("Failure when sending message to BlueBubbles")
		return nil, err
	}

	if res.Status != 200 {
		bb.log.Error().Int64("statusCode", res.Status).Any("response", res).Msg("Failure when sending message to BlueBubbles")
		return nil, fmt.Errorf("could not send message (status %d: %s)", res.Status, res.Message)
	}

	return buildSendResponse(chatID, res.Data), nil
}

func (bb *blueBubbles) UnsendMessage(chatID, targetGUID string, targetPart int) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("targetGUID", targetGUID).Int("targetPart", targetPart).Msg("UnsendMessage")

	if !bb.usingPrivateAPI {
		bb.log.Warn().Str("chatID", chatID).Str("targetGUID", targetGUID).Int("targetPart", targetPart).Msg("The private-api isn't enabled in BlueBubbles, can't unsend message")
		return nil, errors.ErrUnsupported
	}

	request := UnsendMessage{
		PartIndex: targetPart,
	}

	var res UnsendMessageResponse

	err := bb.apiPost("/api/v1/message/"+targetGUID+"/unsend", request, &res)
	if err != nil {
		bb.log.Error().Err(err).Any("response", res).Msg("Failure when unsending message in BlueBubbles")
		return nil, err
	}

	if res.Status != 200 {
		bb.log.Error().Int64("statusCode", res.Status).Any("response", res).Msg("Failure when unsending message in BlueBubbles")
		return nil, fmt.Errorf("could not unsend message (status %d: %s)", res.Status, res.Message)
	}

	return buildSendResponse(chatID, res.Data), nil
}

func (bb *blueBubbles) EditMessage(chatID string, targetGUID string, newText string, targetPart int) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("targetGUID", targetGUID).Str("newText", newText).Int("targetPart", targetPart).Msg("EditMessage")

	if !bb.usingPrivateAPI {
		bb.log.Warn().Str("chatID", chatID).Str("targetGUID", targetGUID).Str("newText", newText).Int("targetPart", targetPart).Msg("The private-api isn't enabled in BlueBubbles, can't edit message")
		return nil, errors.ErrUnsupported
	}

	request := EditMessage{
		EditedMessage:                 newText,
		BackwardsCompatibilityMessage: "Edited to \"" + newText + "\"",
		PartIndex:                     targetPart,
	}

	var res EditMessageResponse

	err := bb.apiPost("/api/v1/message/"+targetGUID+"/edit", request, &res)
	if err != nil {
		bb.log.Error().Err(err).Any("response", res).Msg("Failure when editing message in BlueBubbles")
		return nil, err
	}

	if res.Status != 200 {
		bb.log.Error().Int64("statusCode", res.Status).Any("response", res).Msg("Failure when editing message in BlueBubbles")
		return nil, fmt.Errorf("could not edit message (status %d: %s)", res.Status, res.Message)
	}

	return buildSendResponse(chatID, res.Data), nil
}

func (bb *blueBubbles) isPrivateAPI() bool {
	var serverInfo ServerInfoResponse
	err := bb.apiGet("/api/v1/server/info", nil, &serverInfo)
	if err != nil {
		bb.log.Error().Err(err).Msg("Failed to get server info from BlueBubbles")
		return false
	}

	return serverInfo.Data.PrivateAPI
}

func (bb *blueBubbles) SendFile(chatID, text, filename string, pathOnDisk string, replyTo string, replyToPart int, mimeType string, voiceMemo bool, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("text", text).Str("filename", filename).Str("pathOnDisk", pathOnDisk).Str("replyTo", replyTo).Int("replyToPart", replyToPart).Str("mimeType", mimeType).Bool("voiceMemo", voiceMemo).Interface("metadata", metadata).Msg("SendFile")

	attachment, err := os.ReadFile(pathOnDisk)
	if err != nil {
		return nil, err
	}

	if filename == "" {
		filename = filepath.Base(pathOnDisk)
	}
	if filename == "" || filename == "." || filename == "/" {
		filename = "attachment"
	}

	bb.log.Info().Int("attachmentSize", len(attachment)).Str("filename", filename).Msg("Read attachment from disk")

	var method string
	if bb.usingPrivateAPI {
		method = "private-api"
	} else {
		// we have to use apple-script and send a second message if there's a caption
		method = "apple-script"
	}

	formData := map[string]interface{}{
		"chatGuid":       chatID,
		"tempGuid":       fmt.Sprintf("temp-%s", RandString(8)),
		"name":           filename,
		"method":         method,
		"attachment":     formFile{Filename: filename, Data: attachment},
		"isAudioMessage": voiceMemo,
	}

	if replyTo != "" {
		formData["selectedMessageGuid"] = replyTo
		formData["partIndex"] = replyToPart
	}

	if bb.usingPrivateAPI && text != "" {
		formData["subject"] = text
	}

	path := "/api/v1/message/attachment"

	var response SendTextResponse
	if err := bb.apiPostAsFormData(path, formData, &response); err != nil {
		return nil, err
	}

	if response.Status != 200 {
		bb.log.Error().Int64("statusCode", response.Status).Any("response", response).Msg("Failure when sending attachment to BlueBubbles")
		return nil, fmt.Errorf("could not send attachment (status %d: %s)", response.Status, response.Message)
	}

	if !bb.usingPrivateAPI && text != "" {
		if _, err := bb.SendMessage(chatID, text, replyTo, replyToPart, nil, nil); err != nil {
			bb.log.Warn().Err(err).Str("chatID", chatID).Msg("Failed to send caption text after attachment")
		}
	}

	return buildSendResponse(chatID, response.Data), nil
}

func (bb *blueBubbles) SendFileCleanup(sendFileDir string) {
	_ = os.RemoveAll(sendFileDir)
}

func (bb *blueBubbles) sendReaction(chatID, targetGUID string, targetPart int, reaction string, remove bool) (*imessage.SendResponse, error) {
	if !bb.usingPrivateAPI {
		bb.log.Warn().Str("chatID", chatID).Str("targetGUID", targetGUID).Str("reaction", reaction).Bool("remove", remove).Msg("The private-api isn't enabled in BlueBubbles, can't send tapback")
		return nil, errors.ErrUnsupported
	}

	if reaction == "" {
		return nil, errors.New("empty tapback reaction")
	}

	if remove && !strings.HasPrefix(reaction, "-") {
		reaction = "-" + reaction
	}

	request := SendReactionRequest{
		ChatGUID:            chatID,
		SelectedMessageGUID: targetGUID,
		PartIndex:           targetPart,
		Reaction:            reaction,
	}

	var res SendReactionResponse

	err := bb.apiPost("/api/v1/message/react", request, &res)
	if err != nil {
		return nil, err
	}

	if res.Status != 200 {
		bb.log.Error().Any("response", res).Msg("Failure when sending reaction to BlueBubbles")
		return nil, fmt.Errorf("could not send reaction (status %d: %s)", res.Status, res.Message)
	}

	return buildSendResponse(chatID, res.Data), nil
}

func (bb *blueBubbles) SendTapback(chatID, targetGUID string, targetPart int, tapback imessage.TapbackType, remove bool) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("targetGUID", targetGUID).Int("targetPart", targetPart).Interface("tapback", tapback).Bool("remove", remove).Msg("SendTapback")
	return bb.sendReaction(chatID, targetGUID, targetPart, tapback.Name(), remove)
}

func (bb *blueBubbles) SendCustomEmojiTapback(chatID, targetGUID string, targetPart int, emoji string, remove bool) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("targetGUID", targetGUID).Int("targetPart", targetPart).Str("emoji", emoji).Bool("remove", remove).Msg("SendCustomEmojiTapback")
	return bb.sendReaction(chatID, targetGUID, targetPart, emoji, remove)
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

	if identifierResponse.Data.Service == "" || identifierResponse.Data.Address == "" {
		bb.log.Warn().Any("response", identifierResponse).Str("address", address).Msg("No results found for provided identifier. Assuming 'iMessage' service.")
		return "iMessage;-;" + handle, nil
	}

	return identifierResponse.Data.Service + ";-;" + identifierResponse.Data.Address, nil
}

func (bb *blueBubbles) PrepareDM(guid string) error {
	bb.log.Trace().Str("guid", guid).Msg("PrepareDM")
	return nil
}

func (bb *blueBubbles) CreateGroup(users []string) (*imessage.CreateGroupResponse, error) {
	bb.log.Trace().Interface("users", users).Msg("CreateGroup")

	if !bb.usingPrivateAPI {
		bb.log.Warn().Interface("users", users).Msg("The private-api isn't enabled in BlueBubbles, can't create group")
		return nil, errors.ErrUnsupported
	}
	if len(users) == 0 {
		return nil, errors.New("at least one participant is required to create a group")
	}

	service := "iMessage"
	addresses := make([]string, 0, len(users))
	for _, user := range users {
		if strings.Contains(user, ";") {
			parsed := imessage.ParseIdentifier(user)
			if parsed.Service == "SMS" {
				service = "SMS"
			}
			if parsed.LocalID != "" {
				addresses = append(addresses, parsed.LocalID)
				continue
			}
		}
		if trimmed := strings.TrimSpace(user); trimmed != "" {
			addresses = append(addresses, trimmed)
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("no valid participant addresses provided")
	}

	req := CreateChatRequest{
		Addresses: addresses,
		Service:   service,
		Method:    "private-api",
		TempGUID:  fmt.Sprintf("temp-%s", RandString(8)),
	}

	var res CreateChatResponse
	if err := bb.apiPost("/api/v1/chat/new", req, &res); err != nil {
		bb.log.Error().Err(err).Any("response", res).Msg("Failure when creating group chat in BlueBubbles")
		return nil, err
	}
	if res.Status != 200 || res.Data.GUID == "" {
		bb.log.Error().Int64("statusCode", res.Status).Any("response", res).Msg("Failure when creating group chat in BlueBubbles")
		return nil, fmt.Errorf("could not create group chat (status %d: %s)", res.Status, res.Message)
	}

	return &imessage.CreateGroupResponse{
		GUID:     res.Data.GUID,
		ThreadID: res.Data.GroupID,
	}, nil
}

// Helper functions

type formFile struct {
	Filename string
	Data     []byte
}

func (bb *blueBubbles) joinBasePath(basePath, relPath string) string {
	base := strings.TrimRight(basePath, "/")
	rel := strings.TrimLeft(relPath, "/")
	if base == "" {
		return "/" + rel
	}
	return base + "/" + rel
}

func (bb *blueBubbles) wsUrl() (string, error) {
	rawURL := strings.TrimSpace(bb.bridge.GetConnectorConfig().BlueBubblesURL)
	if rawURL == "" {
		return "", errors.New("bluebubbles_url is not configured")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error parsing BlueBubbles URL")
		return "", fmt.Errorf("invalid bluebubbles_url: %w", err)
	}

	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported bluebubbles_url scheme %q", u.Scheme)
	}

	u.Path = bb.joinBasePath(u.Path, "socket.io/")

	q := u.Query()
	q.Set("guid", bb.bridge.GetConnectorConfig().BlueBubblesPassword)
	q.Set("EIO", "4")
	q.Set("transport", "websocket")
	u.RawQuery = q.Encode()

	return u.String(), nil
}

func (bb *blueBubbles) apiURL(path string, queryParams map[string]string) (string, error) {
	rawURL := strings.TrimSpace(bb.bridge.GetConnectorConfig().BlueBubblesURL)
	if rawURL == "" {
		return "", errors.New("bluebubbles_url is not configured")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error parsing BlueBubbles URL")
		return "", fmt.Errorf("invalid bluebubbles_url: %w", err)
	}

	u.Path = bb.joinBasePath(u.Path, path)

	q := u.Query()
	q.Set("password", bb.bridge.GetConnectorConfig().BlueBubblesPassword)

	for key, value := range queryParams {
		q.Add(key, value)
	}

	u.RawQuery = q.Encode()

	return u.String(), nil
}

func checkHTTPStatus(method, path string, resp *http.Response, body []byte) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 256 {
		snippet = snippet[:256] + "..."
	}
	return fmt.Errorf("BlueBubbles %s %s returned HTTP %d: %s", method, path, resp.StatusCode, snippet)
}

func (bb *blueBubbles) apiGet(path string, queryParams map[string]string, target interface{}) (err error) {
	reqURL, err := bb.apiURL(path, queryParams)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(bb.context(), http.MethodGet, reqURL, nil)
	if err != nil {
		bb.log.Error().Err(err).Str("path", path).Msg("Error creating GET request")
		return err
	}

	bb.bbRequestLock.Lock()
	response, err := bb.getHTTPClient(false).Do(req)
	bb.bbRequestLock.Unlock()
	if err != nil {
		bb.log.Error().Err(err).Str("path", path).Msg("Error making GET request")
		return err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		bb.log.Error().Err(err).Str("path", path).Msg("Error reading response body")
		return err
	}

	if err := checkHTTPStatus(http.MethodGet, path, response, responseBody); err != nil {
		bb.log.Error().Err(err).Int("status", response.StatusCode).Str("path", path).Msg("BlueBubbles GET request failed")
		return err
	}

	if target != nil {
		if err := json.Unmarshal(responseBody, target); err != nil {
			bb.log.Error().Err(err).Str("path", path).Msg("Error unmarshalling response body")
			return err
		}
	}

	return nil
}

func (bb *blueBubbles) apiPost(path string, payload interface{}, target interface{}) error {
	return bb.apiRequest(http.MethodPost, path, payload, target)
}

func (bb *blueBubbles) apiDelete(path string, payload interface{}, target interface{}) error {
	return bb.apiRequest(http.MethodDelete, path, payload, target)
}

func (bb *blueBubbles) apiRequest(method, path string, payload interface{}, target interface{}) (err error) {
	reqURL, err := bb.apiURL(path, nil)
	if err != nil {
		return err
	}

	var bodyReader io.Reader
	if payload != nil {
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			bb.log.Error().Err(err).Str("path", path).Msg("Error marshalling payload")
			return err
		}
		bodyReader = bytes.NewBuffer(payloadJSON)
	}

	req, err := http.NewRequestWithContext(bb.context(), method, reqURL, bodyReader)
	if err != nil {
		bb.log.Error().Err(err).Str("method", method).Str("path", path).Msg("Error creating request")
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	bb.bbRequestLock.Lock()
	response, err := bb.getHTTPClient(false).Do(req)
	bb.bbRequestLock.Unlock()
	if err != nil {
		bb.log.Error().Err(err).Str("method", method).Str("path", path).Msg("Error making request")
		return err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		bb.log.Error().Err(err).Str("path", path).Msg("Error reading response body")
		return err
	}

	if err := checkHTTPStatus(method, path, response, responseBody); err != nil {
		bb.log.Error().Err(err).Int("status", response.StatusCode).Str("method", method).Str("path", path).Msg("BlueBubbles request failed")
		return err
	}

	if target != nil {
		if err := json.Unmarshal(responseBody, target); err != nil {
			bb.log.Error().Err(err).Str("path", path).Msg("Error unmarshalling response body")
			return err
		}
	}

	return nil
}

func (bb *blueBubbles) apiPostAsFormData(path string, formData map[string]interface{}, target interface{}) error {
	reqURL, err := bb.apiURL(path, nil)
	if err != nil {
		return err
	}

	// Create a new buffer to store the file content
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	for key, value := range formData {
		switch v := value.(type) {
		case int, bool:
			if err := writer.WriteField(key, fmt.Sprint(v)); err != nil {
				return err
			}
		case string:
			if err := writer.WriteField(key, v); err != nil {
				return err
			}
		case formFile:
			fname := v.Filename
			if fname == "" {
				fname = "attachment"
			}
			part, err := writer.CreateFormFile(key, fname)
			if err != nil {
				bb.log.Error().Err(err).Msg("Error creating form-data field")
				return err
			}
			if _, err = part.Write(v.Data); err != nil {
				bb.log.Error().Err(err).Msg("Error writing file to form-data")
				return err
			}
		case []byte:
			part, err := writer.CreateFormFile(key, "attachment")
			if err != nil {
				bb.log.Error().Err(err).Msg("Error creating form-data field")
				return err
			}
			if _, err = part.Write(v); err != nil {
				bb.log.Error().Err(err).Msg("Error writing file to form-data")
				return err
			}
		default:
			return fmt.Errorf("unable to serialize %s (type %T) into form-data", key, v)
		}
	}

	// Close the multipart writer
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(bb.context(), http.MethodPost, reqURL, &body)
	if err != nil {
		bb.log.Error().Err(err).Str("path", path).Msg("Error creating multipart POST request")
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Make the HTTP POST request
	bb.bbRequestLock.Lock()
	response, err := bb.getHTTPClient(true).Do(req)
	bb.bbRequestLock.Unlock()
	if err != nil {
		bb.log.Error().Err(err).Str("path", path).Msg("Error making POST request")
		return err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		bb.log.Error().Err(err).Str("path", path).Msg("Error reading response body")
		return err
	}

	if err := checkHTTPStatus(http.MethodPost, path, response, responseBody); err != nil {
		bb.log.Error().Err(err).Int("status", response.StatusCode).Str("path", path).Msg("BlueBubbles multipart POST failed")
		return err
	}

	if target != nil {
		if err := json.Unmarshal(responseBody, target); err != nil {
			bb.log.Error().Err(err).Str("path", path).Msg("Error unmarshalling response body")
			return err
		}
	}

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
	case nil:
		convertedID = ""
	default:
		bb.log.Error().Interface("id", id).Msg("Unknown type for contact ID")
		convertedID = ""
	}

	if bbContact.Avatar != nil && *bbContact.Avatar != "" {
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

func parseThreadOriginatorPart(part string) (int, error) {
	trimmed := strings.TrimSpace(part)
	if trimmed == "" {
		return 0, nil
	}
	parts := strings.Split(trimmed, ":")
	return strconv.Atoi(parts[0])
}

func (bb *blueBubbles) convertBBMessageToiMessage(bbMessage Message) (*imessage.Message, error) {
	if len(bbMessage.Chats) == 0 {
		return nil, fmt.Errorf("BlueBubbles message %s has no chats", bbMessage.GUID)
	}

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
	if message.Service == "" {
		message.Service = imessage.ParseIdentifier(message.ChatGUID).Service
	}
	message.IsFromMe = bbMessage.IsFromMe
	message.IsRead = bbMessage.DateRead != 0
	if message.IsRead {
		message.ReadAt = time.UnixMilli(bbMessage.DateRead)
	}
	message.IsDelivered = bbMessage.DateDelivered != 0
	message.IsSent = bbMessage.DateCreated != 0 // assume yes because we made it to this part of the code
	message.IsEmote = false                     // emojis seem to send either way, and BB doesn't say whether there is one or not
	message.IsAudioMessage = bbMessage.IsAudioMessage
	message.IsEdited = bbMessage.DateEdited != 0
	message.IsRetracted = bbMessage.DateRetracted != 0

	message.ReplyToGUID = bbMessage.ThreadOriginatorGUID
	if bbMessage.ThreadOriginatorPart != "" {
		replyPart, err := parseThreadOriginatorPart(bbMessage.ThreadOriginatorPart)
		if err != nil {
			bb.log.Debug().Err(err).Str("threadOriginatorPart", bbMessage.ThreadOriginatorPart).Msg("Unable to parse ThreadOriginatorPart")
		} else {
			message.ReplyToPart = replyPart
		}
	}

	// Tapbacks (classic 6 + iOS 18 / macOS Sequoia custom emoji tapbacks)
	if bbMessage.AssociatedMessageGUID != "" &&
		(bbMessage.AssociatedMessageType != "" || bbMessage.AssociatedMessageEmoji != "") {
		tbType, customEmoji := bb.convertBBTapbackToImessageTapback(string(bbMessage.AssociatedMessageType), bbMessage.AssociatedMessageEmoji)
		if tbType != 0 {
			isRemove := tbType >= 3000 && tbType < 4000
			isCustomEmoji := tbType == imessage.TapbackCustomEmoji || tbType == imessage.TapbackCustomEmoji+imessage.TapbackRemoveOffset
			if !isCustomEmoji || customEmoji != "" || isRemove {
				tb := &imessage.Tapback{
					TargetGUID:  bbMessage.AssociatedMessageGUID,
					Type:        tbType,
					CustomEmoji: customEmoji,
				}
				if _, parseErr := tb.Parse(); parseErr != nil {
					if !errors.Is(parseErr, imessage.ErrUnknownTapbackTargetType) {
						bb.log.Debug().Err(parseErr).Str("associatedMessageGuid", bbMessage.AssociatedMessageGUID).Msg("Failed to parse tapback target GUID")
						tb = nil
					}
				}
				message.Tapback = tb
			}
		}
	}

	// Attachments
	if len(bbMessage.Attachments) > 0 {
		message.Attachments = make([]*imessage.Attachment, 0, len(bbMessage.Attachments))
		for _, bbAttachment := range bbMessage.Attachments {
			downloaded, err := bb.downloadAttachment(bbAttachment.GUID)
			if err != nil {
				bb.log.Err(err).Str("attachmentGUID", bbAttachment.GUID).Msg("Failed to download attachment")
				continue
			}

			message.Attachments = append(message.Attachments, downloaded)
		}
	}

	// Group name, member, and avatar changes all come through as messages
	// with a special ItemType to denote it isn't a regular message
	message.ItemType = imessage.ItemType(bbMessage.ItemType)

	// Changes based on the ItemType, but denotes user or icon add vs remove actions
	message.GroupActionType = imessage.GroupActionType(bbMessage.GroupActionType)
	message.NewGroupName = bbMessage.GroupTitle

	// TODO Richlinks
	// message.RichLink =

	message.ThreadID = bbMessage.Chats[0].GroupID

	return &message, nil
}

func (bb *blueBubbles) convertBBTapbackToImessageTapback(associatedMessageType, associatedMessageEmoji string) (tbType imessage.TapbackType, customEmoji string) {
	raw := strings.TrimSpace(strings.ToLower(associatedMessageType))

	// Numeric associated message types (2000-2006 add, 3000-3006 remove)
	if n, err := strconv.Atoi(raw); err == nil {
		if (n >= 2000 && n <= 2006) || (n >= 3000 && n <= 3006) {
			tbType = imessage.TapbackType(n)
			if n == int(imessage.TapbackCustomEmoji) || n == int(imessage.TapbackCustomEmoji+imessage.TapbackRemoveOffset) {
				customEmoji = associatedMessageEmoji
			}
			return tbType, customEmoji
		}
		return 0, ""
	}

	remove := strings.HasPrefix(raw, "-")
	name := strings.TrimPrefix(raw, "-")

	switch {
	case strings.Contains(name, "love"):
		tbType = imessage.TapbackLove
	case strings.Contains(name, "dislike"):
		tbType = imessage.TapbackDislike
	case strings.Contains(name, "like"):
		tbType = imessage.TapbackLike
	case strings.Contains(name, "laugh"):
		tbType = imessage.TapbackLaugh
	case strings.Contains(name, "emphasize") || strings.Contains(name, "emphasis"):
		tbType = imessage.TapbackEmphasis
	case strings.Contains(name, "question"):
		tbType = imessage.TapbackQuestion
	case name == "emoji" || associatedMessageEmoji != "":
		tbType = imessage.TapbackCustomEmoji
		customEmoji = associatedMessageEmoji
	default:
		return 0, ""
	}

	if remove {
		tbType += imessage.TapbackRemoveOffset
	}
	return tbType, customEmoji
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
	err = bb.apiGet(fmt.Sprintf("/api/v1/attachment/%s", guid), nil, &attachmentResponse)
	if err != nil {
		bb.log.Err(err).Str("guid", guid).Msg("Failed to get attachment from BlueBubbles")
		return nil, err
	}

	downloadPath := fmt.Sprintf("/api/v1/attachment/%s/download", guid)
	reqURL, err := bb.apiURL(downloadPath, nil)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(bb.context(), http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	response, err := bb.getHTTPClient(true).Do(req)
	if err != nil {
		bb.log.Error().Err(err).Str("guid", guid).Msg("Error downloading attachment from BlueBubbles")
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		bodySnippet, _ := io.ReadAll(io.LimitReader(response.Body, 256))
		err = checkHTTPStatus(http.MethodGet, downloadPath, response, bodySnippet)
		bb.log.Error().Err(err).Str("guid", guid).Msg("BlueBubbles attachment download failed")
		return nil, err
	}

	tempFile, err := os.CreateTemp(os.TempDir(), "bb-attachment-*")
	if err != nil {
		bb.log.Error().Err(err).Msg("Error creating temp file")
		return nil, err
	}
	tempPath := tempFile.Name()

	_, copyErr := io.Copy(tempFile, response.Body)
	closeErr := tempFile.Close()
	if copyErr != nil {
		_ = os.Remove(tempPath)
		bb.log.Error().Err(copyErr).Msg("Error copying response body to temp file")
		return nil, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tempPath)
		bb.log.Error().Err(closeErr).Msg("Error closing temp file")
		return nil, closeErr
	}

	return &imessage.Attachment{
		GUID:       guid,
		PathOnDisk: tempPath,
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
		SendTapbacks:             bb.usingPrivateAPI,
		SendCustomEmojiTapbacks:  bb.usingPrivateAPI,
		UnsendMessages:           bb.usingPrivateAPI,
		EditMessages:             bb.usingPrivateAPI,
		SendReadReceipts:         bb.usingPrivateAPI,
		SendTypingNotifications:  bb.usingPrivateAPI,
		SendCaptions:             true,
		BridgeState:              true,
		MessageStatusCheckpoints: false,
		DeliveredStatus:          bb.usingPrivateAPI,
		ContactChatMerging:       false,
		RichLinks:                false,
		ChatBridgeResult:         false,
	}
}
