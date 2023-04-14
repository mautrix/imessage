// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package imessage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/ipc"
)

var (
	ErrNotLoggedIn = errors.New("you're not logged into iMessage")
)

type ContactAPI interface {
	GetContactInfo(identifier string) (*Contact, error)
	GetContactList() ([]*Contact, error)
}

type ChatInfoAPI interface {
	GetChatInfo(chatID, threadID string) (*ChatInfo, error)
	GetGroupAvatar(chatID string) (*Attachment, error)
}

type API interface {
	Start(readyCallback func()) error
	Stop()
	GetMessagesSinceDate(chatID string, minDate time.Time, backfillID string) ([]*Message, error)
	GetMessagesWithLimit(chatID string, limit int, backfillID string) ([]*Message, error)
	GetChatsWithMessagesAfter(minDate time.Time) ([]ChatIdentifier, error)
	GetMessage(guid string) (*Message, error)
	MessageChan() <-chan *Message
	ReadReceiptChan() <-chan *ReadReceipt
	TypingNotificationChan() <-chan *TypingNotification
	ChatChan() <-chan *ChatInfo
	ContactChan() <-chan *Contact
	MessageStatusChan() <-chan *SendMessageStatus
	BackfillTaskChan() <-chan *BackfillTask
	ContactAPI
	ChatInfoAPI

	ResolveIdentifier(identifier string) (string, error)
	PrepareDM(guid string) error

	SendMessage(chatID, text string, replyTo string, replyToPart int, richLink *RichLink, metadata MessageMetadata) (*SendResponse, error)
	SendFile(chatID, text, filename string, pathOnDisk string, replyTo string, replyToPart int, mimeType string, voiceMemo bool, metadata MessageMetadata) (*SendResponse, error)
	SendFileCleanup(sendFileDir string)
	SendTapback(chatID, targetGUID string, targetPart int, tapback TapbackType, remove bool) (*SendResponse, error)
	SendReadReceipt(chatID, readUpTo string) error
	SendTypingNotification(chatID string, typing bool) error
	SendMessageBridgeResult(chatID, messageID string, eventID id.EventID, success bool)
	SendBackfillResult(chatID, backfillID string, success bool, idMap map[string][]id.EventID)
	SendChatBridgeResult(guid string, mxid id.RoomID)
	NotifyUpcomingMessage(eventID id.EventID)

	PreStartupSyncHook() (StartupSyncHookResponse, error)
	PostStartupSyncHook()

	Capabilities() ConnectorCapabilities
}

var TempFilePermissions os.FileMode = 0640
var TempDirPermissions os.FileMode = 0700

func SendFilePrepare(filename string, data []byte) (string, string, error) {
	dir, err := TempDir("mautrix-imessage-upload")
	if err != nil {
		return "", "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	filePath := filepath.Join(dir, filename)
	err = os.WriteFile(filePath, data, TempFilePermissions)
	if err != nil {
		return "", "", fmt.Errorf("failed to write data to temp file: %w", err)
	}
	return dir, filePath, err
}

type BridgeStatus struct {
	StateEvent string    `json:"state_event"`
	Timestamp  int64     `json:"timestamp"`
	TTL        int       `json:"ttl"`
	Source     string    `json:"source"`
	Error      string    `json:"error,omitempty"`
	Message    string    `json:"message,omitempty"`
	UserID     id.UserID `json:"user_id,omitempty"`
	RemoteID   string    `json:"remote_id,omitempty"`
	RemoteName string    `json:"remote_name,omitempty"`

	Info map[string]interface{} `json:"info,omitempty"`
}

type Bridge interface {
	GetIPC() *ipc.Processor
	GetLog() log.Logger
	GetConnectorConfig() *PlatformConfig
	PingServer() (start, serverTs, end time.Time)
	SendBridgeStatus(state BridgeStatus)
	ReIDPortal(oldGUID, newGUID string, mergeExisting bool) bool
	GetMessagesSince(chatGUID string, since time.Time) []string
	SetPushKey(req *PushKeyRequest)
}

var AppleEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
var Implementations = make(map[string]func(Bridge) (API, error))

type PlatformConfig struct {
	Platform string `yaml:"platform"`

	IMRestPath     string   `yaml:"imessage_rest_path"`
	IMRestArgs     []string `yaml:"imessage_rest_args"`
	ContactsMode   string   `yaml:"contacts_mode"`
	HackySetLocale string   `yaml:"hacky_set_locale"`
	Environment    []string `yaml:"environment"`
	LogIPCPayloads bool     `yaml:"log_ipc_payloads"`
	UnixSocket     string   `yaml:"unix_socket"`

	PingInterval int64 `yaml:"ping_interval_seconds"`
}

func (pc *PlatformConfig) BridgeName() string {
	switch pc.Platform {
	case "android":
		return "Android SMS Bridge"
	default:
		return "iMessage Bridge"
	}
}

func NewAPI(bridge Bridge) (API, error) {
	cfg := bridge.GetConnectorConfig()
	impl, ok := Implementations[cfg.Platform]
	if !ok {
		return nil, fmt.Errorf("no such platform \"%s\", available platforms: %+v", cfg.Platform, Implementations)
	}
	return impl(bridge)
}

func TempDir(name string) (string, error) {
	dir := os.TempDir()
	err := os.MkdirAll(dir, TempDirPermissions)
	if err != nil {
		return "", err
	}
	path, err := os.MkdirTemp(dir, name)
	if err != nil {
		return "", err
	}
	return path, os.Chmod(path, TempDirPermissions)
}
