package bluebubbles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/imessage"
)

type blueBubbles struct {
	bridge            imessage.Bridge
	log               zerolog.Logger
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
	//TODO: automatically configure the webhook within bluebubbles

	//TODO: parameterize the url and port at some point
	http.HandleFunc("/bluebubbles/webhook", bb.webhookHandler)
	go http.ListenAndServe(":8080", nil)
	readyCallback()

	return nil
}

func (bb *blueBubbles) Stop() {
	//TODO: cleanup the webhooks from BBs here
}

type BlueBubblesWebhook struct {
	Data BlueBubblesWebhookData `json:"data"`
	Type string                 `json:"type"`
}

type BlueBubblesWebhookData struct {
	AssociatedMessageGuid string            `json:"associatedMessageGuid,omitempty"`
	AssociatedMessageType interface{}       `json:"associatedMessageType,omitempty"`
	Attachments           []interface{}     `json:"attachments,omitempty"`
	AttributedBody        interface{}       `json:"attributedBody,omitempty"`
	BalloonBundleId       interface{}       `json:"balloonBundleId,omitempty"`
	Chats                 []BlueBubblesChat `json:"chats,omitempty"`
	DateCreated           int64             `json:"dateCreated,omitempty"`
	DateDelivered         int64             `json:"dateDelivered,omitempty"`
	DateEdited            int64             `json:"dateEdited,omitempty"`
	DateRead              int64             `json:"dateRead,omitempty"`
	DateRetracted         int64             `json:"dateRetracted,omitempty"`
	Error                 int               `json:"error,omitempty"`
	ExpressiveSendStyleId interface{}       `json:"expressiveSendStyleId,omitempty"`
	GroupActionType       int               `json:"groupActionType,omitempty"`
	GroupTitle            string            `json:"groupTitle,omitempty"`
	Guid                  string            `json:"guid,omitempty"`
	Handle                BlueBubblesHandle `json:"handle,omitempty"`
	HandleId              int               `json:"handleId,omitempty"`
	HasDdResults          bool              `json:"hasDdResults,omitempty"`
	HasPayloadData        bool              `json:"hasPayloadData,omitempty"`
	IsArchived            bool              `json:"isArchived,omitempty"`
	IsFromMe              bool              `json:"isFromMe,omitempty"`
	ItemType              int               `json:"itemType,omitempty"`
	MessageSummaryInfo    interface{}       `json:"messageSummaryInfo,omitempty"`
	OriginalROWID         int               `json:"originalROWID,omitempty"`
	OtherHandle           int               `json:"otherHandle,omitempty"`
	PartCount             int               `json:"partCount,omitempty"`
	PayloadData           interface{}       `json:"payloadData,omitempty"`
	Subject               string            `json:"subject,omitempty"`
	Text                  string            `json:"text,omitempty"`
	ThreadOriginatorGuid  string            `json:"threadOriginatorGuid,omitempty"`
}

type BlueBubblesChat struct {
	ChatIdentifier string `json:"chatIdentifier,omitempty"`
	DisplayName    string `json:"displayName,omitempty"`
	Guid           string `json:"guid,omitempty"`
	IsArchived     bool   `json:"isArchived,omitempty"`
	OriginalROWID  int    `json:"originalROWID,omitempty"`
	Style          int    `json:"style,omitempty"`
}

type BlueBubblesHandle struct {
	Address           string      `json:"address,omitempty"`
	Country           string      `json:"country,omitempty"`
	OriginalROWID     int         `json:"originalROWID,omitempty"`
	Service           string      `json:"service,omitempty"`
	UncanonicalizedId interface{} `json:"uncanonicalizedId,omitempty"`
}

func (bb *blueBubbles) webhookHandler(w http.ResponseWriter, r *http.Request) {
	// Parse JSON data from BlueBubbles webhook into a generic map
	var webhookData BlueBubblesWebhook
	if err := json.NewDecoder(r.Body).Decode(&webhookData); err != nil {
		// Handle parsing error
		http.Error(w, "Error parsing webhook data", http.StatusBadRequest)
		return
	}

	// Log or inspect the received webhook data
	bb.log.Info().Interface("WebhookData", webhookData).Msg("Received BlueBubbles webhook")

	// Respond to the request (optional)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Webhook received successfully"))

	// Switch based on webhook type
	switch webhookData.Type {
	case "new-message":
		// Handle new message webhook
		bb.handleNewMessage(webhookData.Data)
	case "updated-message":
		// Handle updated message webhook
		bb.handleUpdatedMessage(webhookData.Data)
	case "chat-read-status-changed":
		// Handle chat read status changed webhook
		bb.handleChatReadStatusChanged(webhookData.Data)
	case "typing-indicator":
		// Handle typing indicator webhook
		bb.handleTypingIndicator(webhookData.Data)
	default:
		// Handle unknown webhook type
		bb.log.Warn().Str("Type", webhookData.Type).Msg("Unknown webhook type")
	}
}

// Common Handlers for new events

func (bb *blueBubbles) handleNewMessage(data BlueBubblesWebhookData) {
	var message imessage.Message

	// Convert BlueBubblesWebhookData to imessage.Message
	message.GUID = data.Guid
	message.Time = time.Unix(0, data.DateCreated*int64(time.Millisecond))
	message.Subject = data.Subject
	message.Text = data.Text
	message.ChatGUID = data.Chats[0].Guid
	message.JSONSenderGUID = data.Handle.Address
	message.Sender = imessage.Identifier{
		LocalID: data.Handle.Address,
		Service: data.Handle.Service,
		IsGroup: false,
	}
	// message.JSONTargetGUID = ""
	// message.Target = imessage.ParseIdentifier(data.Guid)
	message.Service = data.Handle.Service
	message.IsFromMe = data.IsFromMe
	message.IsRead = false
	if message.IsRead {
		message.ReadAt = time.Unix(0, data.DateRead*int64(time.Millisecond))
	}
	message.IsDelivered = true
	message.IsSent = true
	message.IsEmote = false
	// message.IsAudioMessage = data.ItemType == AudioMessageType

	// TODO: ReplyTo
	// message.ReplyToGUID = data.ThreadOriginatorGuid
	// message.ReplyToPart = data.PartCount

	// TODO: Tapbacks
	// message.Tapback = nil

	// TODO: Attachments
	// message.Attachments = make([]*imessage.Attachment, len(data.Attachments))
	// for i, blueBubblesAttachment := range data.Attachments {
	// 	message.Attachments[i] = convertAttachment(blueBubblesAttachment)
	// }

	message.GroupActionType = imessage.GroupActionType(data.GroupActionType)
	message.NewGroupName = data.GroupTitle
	// message.Metadata = convertMessageMetadata(data.Metadata)
	message.ThreadID = data.ThreadOriginatorGuid

	// Handle new message logic
	bb.log.Debug().Msg("Handling new message webhook")

	select {
	case bb.messageChan <- &message:
	default:
		bb.log.Warn().Msg("Incoming message buffer is full")
	}
}

func (bb *blueBubbles) handleUpdatedMessage(data BlueBubblesWebhookData) {
	// Handle updated message logic
	bb.log.Info().Msg("Handling updated message webhook")
	// Add your logic to forward data to Matrix or perform other actions
}

func (bb *blueBubbles) handleChatReadStatusChanged(data BlueBubblesWebhookData) {
	// Handle chat read status changed logic
	bb.log.Info().Msg("Handling chat read status changed webhook")
	// Add your logic to forward data to Matrix or perform other actions
}

func (bb *blueBubbles) handleTypingIndicator(data BlueBubblesWebhookData) {
	// Handle typing indicator logic
	bb.log.Info().Msg("Handling typing indicator webhook")
	// Add your logic to forward data to Matrix or perform other actions
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
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetMessage(guid string) (resp *imessage.Message, err error) {
	bb.log.Trace().Str("guid", guid).Msg("GetMessage")
	return nil, ErrNotImplemented
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

func (bb *blueBubbles) apiPost(path string, payload interface{}, target interface{}) (err error) {
	url := bb.apiUrl(path, map[string]string{})

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		bb.log.Error().Err(err).Msg("Error marshalling payload")
		return err
	}

	response, err := http.Post(url, "application/json", bytes.NewBuffer(payloadJSON))
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

	return nil
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

func (bb *blueBubbles) GetContactInfo(identifier string) (*imessage.Contact, error) {
	bb.log.Trace().Str("identifier", identifier).Msg("GetContactInfo")
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetContactList() (resp []*imessage.Contact, err error) {
	bb.log.Trace().Msg("GetContactList")
	var contactResponse ContactResponse

	err = bb.apiPost("/api/v1/contact", nil, &contactResponse)
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

func (bb *blueBubbles) GetChatInfo(chatID, threadID string) (*imessage.ChatInfo, error) {
	bb.log.Trace().Str("chatID", chatID).Str("threadID", threadID).Msg("GetChatInfo")

	var chatResponse ChatResponse

	// DEVNOTE: it doesn't appear we should URL Encode the chatID... 😬
	//          the BlueBubbles API returned 404s, sometimes, with URL encoding
	err := bb.apiGet(fmt.Sprintf("/api/v1/chat/%s", chatID), map[string]string{
		"with": "participants",
	}, &chatResponse)
	if err != nil {
		return nil, err
	}

	if chatResponse.Data == nil {
		return nil, errors.New("chat is missing data payload")
	}

	if chatResponse.Data.GroupID != threadID {
		return nil, errors.New("threadID does not match")
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

// These functions should all be "send" -ing data TO bluebubbles

func (bb *blueBubbles) SendMessage(chatID, text string, replyTo string, replyToPart int, richLink *imessage.RichLink, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {

	url := bb.bridge.GetConnectorConfig().BlueBubblesURL + "/api/v1/message/text?password=" + bb.bridge.GetConnectorConfig().BlueBubblesPassword
	method := "POST"

	payload := strings.NewReader(fmt.Sprintf(`{
    "chatGuid": "%s",
    "tempGuid": "",
    "message": "%s",
    "method": "apple-script",
    "selectedMessageGuid": "%s"
}`, chatID, text, replyTo))

	client := &http.Client{}
	req, err := http.NewRequest(method, url, payload)

	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		bb.log.Err(err)
		return nil, err
	}
	defer res.Body.Close()

	_, err = io.ReadAll(res.Body)
	if err != nil {
		bb.log.Err(err)
		return nil, err
	}

	bb.log.Print("Sent a message!")

	return nil, nil
}

func (bb *blueBubbles) SendFile(chatID, text, filename string, pathOnDisk string, replyTo string, replyToPart int, mimeType string, voiceMemo bool, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("text", text).Str("filename", filename).Str("pathOnDisk", pathOnDisk).Str("replyTo", replyTo).Int("replyToPart", replyToPart).Str("mimeType", mimeType).Bool("voiceMemo", voiceMemo).Interface("metadata", metadata).Msg("SendFile")
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) SendFileCleanup(sendFileDir string) {
	_ = os.RemoveAll(sendFileDir)
}

func (bb *blueBubbles) SendTapback(chatID, targetGUID string, targetPart int, tapback imessage.TapbackType, remove bool) (*imessage.SendResponse, error) {
	bb.log.Trace().Str("chatID", chatID).Str("targetGUID", targetGUID).Int("targetPart", targetPart).Interface("tapback", tapback).Bool("remove", remove).Msg("SendTapback")
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) SendReadReceipt(chatID, readUpTo string) error {
	bb.log.Trace().Str("chatID", chatID).Str("readUpTo", readUpTo).Msg("SendReadReceipt")
	return ErrNotImplemented
}

func (bb *blueBubbles) SendTypingNotification(chatID string, typing bool) error {
	bb.log.Trace().Str("chatID", chatID).Bool("typing", typing).Msg("SendTypingNotification")
	return ErrNotImplemented
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
		MessageSendResponses:     false,
		SendTapbacks:             false,
		SendReadReceipts:         false,
		SendTypingNotifications:  false,
		SendCaptions:             false,
		BridgeState:              false,
		MessageStatusCheckpoints: false,
		DeliveredStatus:          false,
		ContactChatMerging:       false,
		RichLinks:                false,
		ChatBridgeResult:         false,
	}
}
