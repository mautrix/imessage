package bluebubbles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
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
	GroupNameChanged      string = "group-name-changed"
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

	ws, _, err := websocket.DefaultDialer.Dial(bb.wsUrl(), nil)
	if err != nil {
		return err
	}
	err = ws.WriteMessage(websocket.TextMessage, []byte("40"))
	if err != nil {
		return err
	}
	bb.ws = ws

	go bb.PollForWebsocketMessages()

	return nil
}

func (bb *blueBubbles) Stop() {
	bb.log.Trace().Msg("Stop")
	bb.ws.WriteMessage(websocket.CloseMessage, []byte{})
}

func (bb *blueBubbles) PollForWebsocketMessages() {
	defer func() {
		bb.ws.Close()
	}()

	for {
		_, payload, err := bb.ws.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				bb.log.Error().Err(err).Msg("Error reading message from BlueBubbles websocket")
			}
			break
		}

		if bytes.Equal(payload, []byte("2")) {
			bb.log.Debug().Msg("Received ping from BlueBubbles websocket")
			bb.ws.WriteMessage(websocket.TextMessage, []byte("3"))
			continue
		}

		if bytes.HasPrefix(payload, []byte("42")) {
			payload = bytes.TrimPrefix(payload, []byte("42"))

			var incomingWebsocketMessage []json.RawMessage
			if err := json.Unmarshal(payload, &incomingWebsocketMessage); err != nil {
				bb.log.Error().Err(err).Msg("Error parsing message from BlueBubbles websocket")
				continue
			}

			var websocketMessageType string
			if err := json.Unmarshal(incomingWebsocketMessage[0], &websocketMessageType); err != nil {
				bb.log.Error().Err(err).Msg("Error parsing message type from BlueBubbles websocket")
				continue
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
	}
}

func (bb *blueBubbles) handleNewMessage(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleNewMessage")

	var data Message
	err = json.Unmarshal(rawMessage, &data)
	if err != nil {
		return err
	}

	message, err := bb.convertBBMessageToiMessage(data)

	if err != nil {
		return err
	}

	select {
	case bb.messageChan <- message:
	default:
		bb.log.Warn().Msg("Incoming message buffer is full")
	}

	return nil
}

func (bb *blueBubbles) handleMessageSendError(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleMessageSendError")
	return ErrNotImplemented
}

func (bb *blueBubbles) handleMessageUpdated(rawMessage json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("rawMessage", rawMessage).Msg("handleMessageUpdated")

	//TODO: This code words just fine, unless you send a caption with a picture. then it duplicates your message in the matrix client (not visible to the other imessage user though)
	// 		Let's get the multipart message working before we worry about this function
	var data Message
	err = json.Unmarshal(rawMessage, &data)
	if err != nil {
		return err
	}

	message, err := bb.convertBBMessageToiMessage(data)

	if err != nil {
		return err
	}

	select {
	case bb.messageChan <- message:
	default:
		bb.log.Warn().Msg("Incoming message buffer is full")
	}

	return nil
}

func (bb *blueBubbles) handleParticipantRemoved(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleParticipantRemoved")
	return ErrNotImplemented
}

func (bb *blueBubbles) handleParticipantAdded(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleParticipantAdded")
	return ErrNotImplemented
}

func (bb *blueBubbles) handleParticipantLeft(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleParticipantLeft")
	return ErrNotImplemented
}

func (bb *blueBubbles) handleGroupIconChanged(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleGroupIconChanged")
	return ErrNotImplemented
}

func (bb *blueBubbles) handleGroupIconRemoved(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleGroupIconRemoved")
	return ErrNotImplemented
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
		bb.log.Warn().Err(err).Msg("Failed to get chat info")
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
		IsFromMe:       false,                      // changing this to false as I believe read reciepts will always be from others
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

func (bb *blueBubbles) handleGroupNameChanged(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleGroupNameChanged")
	return ErrNotImplemented
}

func (bb *blueBubbles) handleIMessageAliasRemoved(data json.RawMessage) (err error) {
	bb.log.Trace().RawJSON("data", data).Msg("handleIMessageAliasRemoved")
	return ErrNotImplemented
}

// These functions should all be "get" -ting data FROM bluebubbles

var ErrNotImplemented = errors.New("not implemented")

func (bb *blueBubbles) GetMessagesSinceDate(chatID string, minDate time.Time, backfillID string) ([]*imessage.Message, error) {
	bb.log.Trace().Str("chatID", chatID).Time("minDate", minDate).Str("backfillID", backfillID).Msg("GetMessagesSinceDate")
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetMessagesBetween(chatID string, minDate, maxDate time.Time) ([]*imessage.Message, error) {
	bb.log.Trace().Str("chatID", chatID).Time("minDate", minDate).Time("maxDate", maxDate).Msg("GetMessagesBetween")
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetMessagesBeforeWithLimit(chatID string, before time.Time, limit int) ([]*imessage.Message, error) {
	bb.log.Trace().Str("chatID", chatID).Time("before", before).Int("limit", limit).Msg("GetMessagesBeforeWithLimit")
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetMessagesWithLimit(chatID string, limit int, backfillID string) ([]*imessage.Message, error) {
	bb.log.Trace().Str("chatID", chatID).Int("limit", limit).Str("backfillID", backfillID).Msg("GetMessagesWithLimit")

	var messageResponse GetMessagesResponse

	err := bb.apiGet(fmt.Sprintf("/api/v1/chat/%s/message", chatID), map[string]string{
		"limit": strconv.Itoa(limit),
		"sort":  "DESC",
	}, &messageResponse)
	if err != nil {
		bb.log.Err(err).Str("chatID", chatID).Int("limit", limit).Str("backfillID", backfillID).Msg("Failed to get messages from BlueBubbles")
		return nil, err
	}

	var imessages []*imessage.Message

	for _, bbMessage := range messageResponse.Data {
		imessage, err := bb.convertBBMessageToiMessage(bbMessage)

		if err != nil {
			bb.log.Err(err).Str("chatID", chatID).Msg("Failed to convert message from BlueBubbles format to Matrix format")
			return nil, err
		}

		imessages = append(imessages, imessage)
	}

	// Beeper's client will display the messages in the order of this list, even though it has the timestamps
	// to get the most recent `limit` of messages, BB has to return in DESC order,
	// but we need to reverse that so beeper shows them correctly
	imessages = reverseList(imessages)

	return imessages, nil
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

func (bb *blueBubbles) GetChatsWithMessagesAfter(minDate time.Time) (resp []imessage.ChatIdentifier, err error) {
	bb.log.Trace().Time("minDate", minDate).Msg("GetChatsWithMessagesAfter")
	// TODO: find out how to make queries based on minDate and the bluebubbles API
	// TODO: pagination
	limit := int64(5)
	offset := int64(0)

	request := ChatQueryRequest{
		Limit:  limit,
		Offset: offset,
		With: []ChatQueryWith{
			ChatQueryWithLastMessage,
			ChatQueryWithSMS,
		},
		Sort: QuerySortLastMessage,
	}
	var response ChatQueryResponse

	err = bb.apiPost("/api/v1/chat/query", request, &response)
	if err != nil {
		return nil, err
	}

	for _, chat := range response.Data {
		resp = append(resp, imessage.ChatIdentifier{
			ChatGUID: chat.GUID,
			ThreadID: chat.GroupID,
		})
	}

	return resp, nil
}

func (bb *blueBubbles) GetContactInfo(identifier string) (resp *imessage.Contact, err error) {
	bb.log.Trace().Str("identifier", identifier).Msg("GetContactInfo")

	request := ContactQueryRequest{
		Addresses: []string{
			identifier,
		},
	}

	var contactResponse ContactResponse

	err = bb.apiPost("/api/v1/contact/query", request, &contactResponse)
	if err != nil {
		return nil, err
	}

	// Convert to imessage.Contact type
	if len(contactResponse.Data) == 1 {
		for _, contact := range contactResponse.Data {
			resp = &imessage.Contact{
				FirstName: contact.FirstName,
				LastName:  contact.LastName,
				Nickname:  contact.Nickname,
				Phones:    convertPhones(contact.PhoneNumbers),
				Emails:    convertEmails(contact.Emails),
				UserGUID:  contact.ID,
			}
			return resp, nil
		}
	} else if len(contactResponse.Data) > 1 {
		err = errors.New("too many contacts found")
		bb.log.Err(err).Int("contactCount", len(contactResponse.Data)).Msg("Expected only a single contact to match, aborting contact retrieval")
		return nil, err
	} else {
		err = errors.New("no contacts found for address")
		bb.log.Err(err).Int("contactCount", len(contactResponse.Data)).Msg("Expected only a single contact to match, aborting contact retrieval")
		return nil, err
	}

	return nil, errors.New("if you see this message, then math no longer makes sense")
}

func (bb *blueBubbles) GetContactList() (resp []*imessage.Contact, err error) {
	bb.log.Trace().Msg("GetContactList")

	var contactResponse ContactResponse

	err = bb.apiGet("/api/v1/contact", nil, &contactResponse)
	if err != nil {
		return nil, err
	}

	// Convert to imessage.Contact type
	for _, contact := range contactResponse.Data {
		imessageContact := &imessage.Contact{
			FirstName: contact.FirstName,
			LastName:  contact.LastName,
			Nickname:  contact.Nickname,
			Phones:    convertPhones(contact.PhoneNumbers),
			Emails:    convertEmails(contact.Emails),
			UserGUID:  contact.ID,
		}
		resp = append(resp, imessageContact)
	}

	return resp, nil
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
		return nil, err
	}

	members := make([]string, len(chatResponse.Data.Partipants))

	for i, participant := range chatResponse.Data.Partipants {
		members[i] = participant.Address
	}

	chatInfo := &imessage.ChatInfo{
		JSONChatGUID: chatResponse.Data.GUID,
		Identifier:   imessage.ParseIdentifier(chatResponse.Data.GUID),
		DisplayName:  chatResponse.Data.DisplayName,
		Members:      members,
		ThreadID:     chatResponse.Data.GroupID,
	}

	return chatInfo, nil
}

func (bb *blueBubbles) GetGroupAvatar(chatID string) (*imessage.Attachment, error) {
	bb.log.Trace().Str("chatID", chatID).Msg("GetGroupAvatar")
	return nil, ErrNotImplemented
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

	request := SendTextRequest{
		ChatGUID:            chatID,
		Method:              "apple-script",
		Message:             text,
		TempGuid:            fmt.Sprintf("temp-%s", RandString(8)),
		SelectedMessageGuid: replyTo,
		PartIndex:           &replyToPart,
	}

	var res SendTextResponse

	err := bb.apiPost("/api/v1/message/text", request, &res)
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
		Time:    time.Unix(0, res.Data.DateCreated*int64(time.Millisecond)),
	}, nil
}

func (bb *blueBubbles) SendFile(chatID, text, filename string, pathOnDisk string, replyTo string, replyToPart int, mimeType string, voiceMemo bool, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("text", text).Str("filename", filename).Str("pathOnDisk", pathOnDisk).Str("replyTo", replyTo).Int("replyToPart", replyToPart).Str("mimeType", mimeType).Bool("voiceMemo", voiceMemo).Interface("metadata", metadata).Msg("SendFile")

	// Prepare the payload for the POST request
	payload := map[string]interface{}{
		"chatGuid":            chatID,
		"name":                filename,
		"method":              "private-api",
		"selectedMessageGuid": replyTo,
		"partIndex":           fmt.Sprint(replyToPart),
	}

	// Define the API endpoint path
	path := "/api/v1/message/attachment"

	// Make the API POST request with file
	var response SendTextResponse
	if err := bb.apiPostWithFile(path, payload, pathOnDisk, &response); err != nil {
		return nil, err
	}

	if response.Status == 200 {
		bb.log.Debug().Msg("Sent a file!")

		bb.SendMessage(chatID, text, replyTo, replyToPart, nil, nil)

		var imessageSendResponse = imessage.SendResponse{
			GUID:    response.Data.GUID,
			Service: response.Data.Handle.Service,
			Time:    time.Unix(0, response.Data.DateCreated*int64(time.Millisecond)),
		}

		return &imessageSendResponse, nil
	} else {
		bb.log.Error().Any("response", response).Msg("Failure when sending message to BlueBubbles")

		return nil, errors.New("could not send message")
	}
}

func (bb *blueBubbles) SendFileCleanup(sendFileDir string) {
	_ = os.RemoveAll(sendFileDir)
}

func (bb *blueBubbles) SendTapback(chatID, targetGUID string, targetPart int, tapback imessage.TapbackType, remove bool) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("targetGUID", targetGUID).Int("targetPart", targetPart).Interface("tapback", tapback).Bool("remove", remove).Msg("SendTapback")

	request := SendReactionRequest{
		ChatGUID:            chatID,
		SelectedMessageGuid: targetGUID,
		PartIndex:           targetPart,
		Reaction:            tapback.Name(),
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
		Time:    time.Unix(0, res.Data.DateCreated*int64(time.Millisecond)),
	}, nil
}

func (bb *blueBubbles) SendReadReceipt(chatID, readUpTo string) error {
	bb.log.Trace().Str("chatID", chatID).Str("readUpTo", readUpTo).Msg("SendReadReceipt")

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

func (bb *blueBubbles) ResolveIdentifier(identifier string) (string, error) {
	bb.log.Trace().Str("identifier", identifier).Msg("ResolveIdentifier")
	return "", ErrNotImplemented
}

func (bb *blueBubbles) PrepareDM(guid string) error {
	bb.log.Trace().Str("guid", guid).Msg("PrepareDM")
	return ErrNotImplemented
}

func (bb *blueBubbles) CreateGroup(users []string) (*imessage.CreateGroupResponse, error) {
	bb.log.Trace().Interface("users", users).Msg("CreateGroup")
	return nil, ErrNotImplemented
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

func (bb *blueBubbles) apiUrl(path string, queryParams map[string]string) string {
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
	url := bb.apiUrl(path, queryParams)

	response, err := http.Get(url)
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
	url := bb.apiUrl(path, map[string]string{})

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
	response, err := client.Do(req)
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

func (bb *blueBubbles) apiPostWithFile(path string, params map[string]interface{}, filePath string, target interface{}) error {
	url := bb.apiUrl(path, map[string]string{})

	// Create a new buffer to store the file content
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add the file to the request
	fileWriter, err := writer.CreateFormFile("attachment", filepath.Base(filePath))
	if err != nil {
		bb.log.Error().Err(err).Msg("Error creating form file")
		return err
	}

	file, err := os.Open(filePath)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error opening file")
		return err
	}
	defer file.Close()

	_, err = io.Copy(fileWriter, file)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error copying file content")
		return err
	}

	// Add other parameters to the request
	for key, value := range params {
		_ = writer.WriteField(key, fmt.Sprint(value))
	}

	// Close the multipart writer
	writer.Close()

	// Make the HTTP POST request
	response, err := http.Post(url, writer.FormDataContentType(), &body)
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

func (bb *blueBubbles) convertBBMessageToiMessage(bbMessage Message) (*imessage.Message, error) {

	var message imessage.Message

	// Convert bluebubbles.Message to imessage.Message
	message.GUID = bbMessage.GUID
	message.Time = time.Unix(0, bbMessage.DateCreated*int64(time.Millisecond))
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
		message.ReadAt = time.Unix(0, bbMessage.DateRead*int64(time.Millisecond))
	}
	message.IsDelivered = bbMessage.DateDelivered != 0 // TODO this may need to be based on the bbMessage.DateDelivered != 0
	message.IsSent = true                              // assume yes because we made it to this part of the code
	message.IsEmote = false                            // emojis seem to send either way, and BB doesn't say whether there is one or not
	message.IsAudioMessage = bbMessage.IsAudioMessage

	message.ReplyToGUID = bbMessage.ThreadOriginatorGuid

	// TODO: ReplyToPart from bluebubbles looks like "0:0:17" in one test I did
	// 		I don't know what the value means, or how to parse it
	// num, err := strconv.Atoi(bbMessage.ThreadOriginatorPart)
	// if err != nil {
	// 	bb.log.Err(err).Str("ThreadOriginatorPart", bbMessage.ThreadOriginatorPart).Msg("Unable to convert ThreadOriginatorPart to an int")
	// } else {
	// 	message.ReplyToPart = num
	// }

	// Tapbacks
	if bbMessage.AssociatedMessageGuid != "" &&
		bbMessage.AssociatedMessageType != "" {
		message.Tapback = &imessage.Tapback{
			TargetGUID: bbMessage.AssociatedMessageGuid,
			Type:       imessage.TapbackFromName(bbMessage.AssociatedMessageType),
		}
		message.Tapback.Parse()
	} else {
		message.Tapback = nil
	}

	message.Attachments = make([]*imessage.Attachment, len(bbMessage.Attachments))
	for i, blueBubblesAttachment := range bbMessage.Attachments {
		attachment, err := bb.convertAttachment(blueBubblesAttachment)
		if err != nil {
			bb.log.Warn().Err(err).Msg("Error converting attachment")
			continue
		}
		message.Attachments[i] = attachment
	}

	// TODO Not sure what the itemtype is
	// message.ItemType =

	message.GroupActionType = imessage.GroupActionType(bbMessage.GroupActionType)
	message.NewGroupName = bbMessage.GroupTitle

	// TODO Richlinks
	// message.RichLink =

	message.ThreadID = bbMessage.ThreadOriginatorGuid

	return &message, nil
}

func (bb *blueBubbles) convertAttachment(attachment Attachment) (*imessage.Attachment, error) {
	url := bb.apiUrl(fmt.Sprintf("/api/v1/attachment/%s/download", attachment.GUID), map[string]string{})

	response, err := http.Get(url)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error making GET request")
		return nil, err
	}
	defer response.Body.Close()

	// Create a temp file and read the response body into it
	tempFile, err := os.CreateTemp(os.TempDir(), attachment.GUID)
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
		GUID:       attachment.GUID,
		PathOnDisk: tempFile.Name(),
		FileName:   attachment.TransferName,
		MimeType:   attachment.MimeType,
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

// Helper function to convert email addresses
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
	return
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
