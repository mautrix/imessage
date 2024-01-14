package bluebubbles

import (
	"errors"
	"os"
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
	readyCallback()
	return nil
}

func (bb *blueBubbles) Stop() {}

var ErrNotImplemented = errors.New("not implemented")

func (bb *blueBubbles) GetMessagesSinceDate(chatID string, minDate time.Time, backfillID string) ([]*imessage.Message, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetMessagesBetween(chatID string, minDate, maxDate time.Time) ([]*imessage.Message, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetMessagesBeforeWithLimit(chatID string, before time.Time, limit int) ([]*imessage.Message, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetMessagesWithLimit(chatID string, limit int, backfillID string) ([]*imessage.Message, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetMessage(guid string) (resp *imessage.Message, err error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetChatsWithMessagesAfter(minDate time.Time) (resp []imessage.ChatIdentifier, err error) {
	return nil, ErrNotImplemented
}

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

func (bb *blueBubbles) GetContactInfo(identifier string) (*imessage.Contact, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetContactList() ([]*imessage.Contact, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetChatInfo(chatID, threadID string) (*imessage.ChatInfo, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) GetGroupAvatar(chatID string) (*imessage.Attachment, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) SendMessage(chatID, text string, replyTo string, replyToPart int, richLink *imessage.RichLink, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) SendFile(chatID, text, filename string, pathOnDisk string, replyTo string, replyToPart int, mimeType string, voiceMemo bool, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) SendFileCleanup(sendFileDir string) {
	_ = os.RemoveAll(sendFileDir)
}

func (bb *blueBubbles) SendTapback(chatID, targetGUID string, targetPart int, tapback imessage.TapbackType, remove bool) (*imessage.SendResponse, error) {
	return nil, ErrNotImplemented
}

func (bb *blueBubbles) SendReadReceipt(chatID, readUpTo string) error {
	return ErrNotImplemented
}

func (bb *blueBubbles) SendTypingNotification(chatID string, typing bool) error {
	return ErrNotImplemented
}

func (bb *blueBubbles) ResolveIdentifier(identifier string) (string, error) {
	return "", ErrNotImplemented
}

func (bb *blueBubbles) PrepareDM(guid string) error {
	return ErrNotImplemented
}

func (bb *blueBubbles) CreateGroup(users []string) (*imessage.CreateGroupResponse, error) {
	return nil, ErrNotImplemented
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
