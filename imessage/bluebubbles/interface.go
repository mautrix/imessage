package bluebubbles

type PageMetadata struct {
	Count  int64 `json:"count"`
	Total  int64 `json:"total"`
	Offset int64 `json:"offset"`
	Limit  int64 `json:"limit"`
}

type MessageQuerySort string

const (
	MessageQuerySortAsc  MessageQuerySort = "ASC"
	MessageQuerySortDesc MessageQuerySort = "DESC"
)

type MessageQueryRequest struct {
	// TODO Other Fields
	ChatGUID string             `json:"chatGuid"`
	Limit    int64              `json:"limit"`
	Offset   int64              `json:"offset"`
	With     []MessageQueryWith `json:"with"`
	Sort     MessageQuerySort   `json:"sort"`
	Before   *int64             `json:"before,omitempty"`
	After    *int64             `json:"after,omitempty"`
}

type MessageQueryWith string

const (
	MessageQueryWithChat             ChatQueryWith = "chat"
	MessageQueryWithChatParticipants ChatQueryWith = "chat.participants"
	MessageQueryWithAttachment       ChatQueryWith = "attachment"
	MessageQueryWithHandle           ChatQueryWith = "handle"
	MessageQueryWithSms              ChatQueryWith = "sms"
)

type MessageQueryResponse struct {
	Status   int64        `json:"status"`
	Message  string       `json:"message"`
	Data     []Message    `json:"data"`
	Metadata PageMetadata `json:"metadata"`
}

type ChatQuerySort string

const (
	QuerySortLastMessage ChatQuerySort = "lastmessage"
)

type ChatQueryRequest struct {
	// TODO Other Fields
	Limit  int64           `json:"limit"`
	Offset int64           `json:"offset"`
	With   []ChatQueryWith `json:"with"`
	Sort   ChatQuerySort   `json:"sort"`
}

type ChatQueryWith string

const (
	ChatQueryWithSMS         ChatQueryWith = "sms"
	ChatQueryWithLastMessage ChatQueryWith = "lastMessage"
)

type ChatQueryResponse struct {
	Status   int64        `json:"status"`
	Message  string       `json:"message"`
	Data     []Chat       `json:"data"`
	Metadata PageMetadata `json:"metadata"`
}

type ChatResponse struct {
	Status  int64  `json:"status"`
	Message string `json:"message"`
	Data    *Chat  `json:"data,omitempty"`
}

type Chat struct {
	// TODO How to get timestamp
	GUID           string           `json:"guid"`
	ChatIdentifier string           `json:"chatIdentifier"`
	GroupID        string           `json:"groupId,omitempty"`
	DisplayName    string           `json:"displayName"`
	Partipants     []Participant    `json:"participants"`
	LastMessage    *Message         `json:"lastMessage,omitempty"`
	Properties     []ChatProperties `json:"properties,omitempty"`
}

type ChatProperties struct {
	GroupPhotoGuid *string `json:"groupPhotoGuid,omitempty"`
}

type Participant struct {
	Address string `json:"address"`
}

type ContactQueryRequest struct {
	Addresses []string `json:"addresses"`
}

type ContactResponse struct {
	Status  int64     `json:"status"`
	Message string    `json:"message"`
	Data    []Contact `json:"data"`
}

type Contact struct {
	PhoneNumbers []PhoneNumber `json:"phoneNumbers,omitempty"`
	Emails       []Email       `json:"emails,omitempty"`
	FirstName    string        `json:"firstName,omitempty"`
	LastName     string        `json:"lastName,omitempty"`
	DisplayName  string        `json:"displayName,omitempty"`
	Nickname     string        `json:"nickname,omitempty"`
	Birthday     string        `json:"birthday,omitempty"`
	Avatar       *string       `json:"avatar,omitempty"`
	SourceType   string        `json:"sourceType,omitempty"`
	// DEVNOTE: This field is almost always a string,
	// but at least one user was getting an error because this was a number for them
	ID any `json:"id,omitempty"`
}

type PhoneNumber struct {
	Address string `json:"address,omitempty"`
	ID      any    `json:"id,omitempty"`
}

type Email struct {
	Address string `json:"address,omitempty"`
	ID      any    `json:"id,omitempty"`
}

type TypingNotification struct {
	Display bool   `json:"display"`
	GUID    string `json:"guid"`
}

type Message struct {
	AssociatedMessageGuid     string       `json:"associatedMessageGuid,omitempty"`
	AssociatedMessageType     string       `json:"associatedMessageType,omitempty"`
	Attachments               []Attachment `json:"attachments,omitempty"`
	AttributedBody            []any        `json:"attributedBody,omitempty"`
	BalloonBundleId           any          `json:"balloonBundleId,omitempty"`
	Chats                     []Chat       `json:"chats,omitempty"`
	DateCreated               int64        `json:"dateCreated,omitempty"`
	DateDelivered             int64        `json:"dateDelivered,omitempty"`
	DateEdited                int64        `json:"dateEdited,omitempty"`
	DateRead                  int64        `json:"dateRead,omitempty"`
	DateRetracted             int64        `json:"dateRetracted,omitempty"`
	Error                     int          `json:"error,omitempty"`
	ExpressiveSendStyleId     any          `json:"expressiveSendStyleId,omitempty"`
	GroupActionType           int          `json:"groupActionType,omitempty"`
	GroupTitle                string       `json:"groupTitle,omitempty"`
	GUID                      string       `json:"guid,omitempty"`
	Handle                    Handle       `json:"handle,omitempty"`
	HandleId                  int          `json:"handleId,omitempty"`
	HasDdResults              bool         `json:"hasDdResults,omitempty"`
	HasPayloadData            bool         `json:"hasPayloadData,omitempty"`
	IsArchived                bool         `json:"isArchived,omitempty"`
	IsAudioMessage            bool         `json:"isAudioMessage,omitempty"`
	IsAutoReply               bool         `json:"isAutoReply,omitempty"`
	IsCorrupt                 bool         `json:"isCorrupt,omitempty"`
	IsDelayed                 bool         `json:"isDelayed,omitempty"`
	IsExpired                 bool         `json:"isExpired,omitempty"`
	IsForward                 bool         `json:"isForward,omitempty"`
	IsFromMe                  bool         `json:"isFromMe,omitempty"`
	IsServiceMessage          bool         `json:"isServiceMessage,omitempty"`
	IsSpam                    bool         `json:"isSpam,omitempty"`
	IsSystemMessage           bool         `json:"isSystemMessage,omitempty"`
	ItemType                  int          `json:"itemType,omitempty"`
	MessageSummaryInfo        any          `json:"messageSummaryInfo,omitempty"`
	OriginalROWID             int          `json:"originalROWID,omitempty"`
	OtherHandle               int          `json:"otherHandle,omitempty"`
	PartCount                 int          `json:"partCount,omitempty"`
	PayloadData               any          `json:"payloadData,omitempty"`
	ReplyToGuid               string       `json:"replyToGuid,omitempty"`
	ShareDirection            int          `json:"shareDirection,omitempty"`
	ShareStatus               int          `json:"shareStatus,omitempty"`
	Subject                   string       `json:"subject,omitempty"`
	Text                      string       `json:"text,omitempty"`
	ThreadOriginatorGuid      string       `json:"threadOriginatorGuid,omitempty"`
	ThreadOriginatorPart      string       `json:"threadOriginatorPart,omitempty"`
	TimeExpressiveSendStyleId any          `json:"timeExpressiveSendStyleId,omitempty"`
	WasDeliveredQuietly       bool         `json:"wasDeliveredQuietly,omitempty"`
}

type Attachment struct {
	OriginalRowID  int    `json:"originalROWID,omitempty"`
	GUID           string `json:"guid,omitempty"`
	UTI            string `json:"uti,omitempty"`
	MimeType       string `json:"mimeType,omitempty"`
	TransferName   string `json:"transferName,omitempty"`
	TotalBytes     int64  `json:"totalBytes,omitempty"`
	TransferState  int    `json:"transferState,omitempty"`
	IsOutgoing     bool   `json:"isOutgoing,omitempty"`
	HideAttachment bool   `json:"hideAttachment,omitempty"`
	IsSticker      bool   `json:"isSticker,omitempty"`
	OriginalGUID   string `json:"originalGuid,omitempty"`
	HasLivePhoto   bool   `json:"hasLivePhoto,omitempty"`
	Height         int64  `json:"height,omitempty"`
	Width          int64  `json:"width,omitempty"`
	Metadata       any    `json:"metadata,omitempty"`
}

type AttachmentResponse struct {
	Status  int64      `json:"status"`
	Message string     `json:"message"`
	Data    Attachment `json:"data"`
}

type GetMessagesResponse struct {
	Status  int64     `json:"status"`
	Message string    `json:"message"`
	Data    []Message `json:"data"`
	Error   any       `json:"error,omitempty"`
}

type MessageResponse struct {
	Status  int64   `json:"status"`
	Message string  `json:"message"`
	Data    Message `json:"data"`
	Error   any     `json:"error,omitempty"`
}

type Handle struct {
	Address           string `json:"address,omitempty"`
	Country           string `json:"country,omitempty"`
	OriginalROWID     int    `json:"originalROWID,omitempty"`
	Service           string `json:"service,omitempty"`
	UncanonicalizedId any    `json:"uncanonicalizedId,omitempty"`
}

type SendTextRequest struct {
	ChatGUID            string `json:"chatGuid"`
	TempGuid            string `json:"tempGuid"`
	Method              string `json:"method"`
	Message             string `json:"message"`
	EffectId            string `json:"effectId,omitempty"`
	Subject             string `json:"subject,omitempty"`
	SelectedMessageGuid string `json:"selectedMessageGuid,omitempty"`
	PartIndex           int    `json:"partIndex,omitempty"`
}

type SendTextResponse struct {
	Status  int64   `json:"status"`
	Message string  `json:"message"`
	Data    Message `json:"data,omitempty"`
	Error   any     `json:"error,omitempty"`
}

type SendReactionRequest struct {
	ChatGUID            string `json:"chatGuid"`
	Reaction            string `json:"reaction"`
	SelectedMessageGuid string `json:"selectedMessageGuid"`
	PartIndex           int    `json:"partIndex"`
}

type SendReactionResponse struct {
	Status  int64   `json:"status"`
	Message string  `json:"message"`
	Data    Message `json:"data,omitempty"`
	Error   any     `json:"error"`
}

type ReadReceiptResponse struct {
	Status  int64  `json:"status"`
	Message string `json:"message"`
	Error   any    `json:"error"`
}

type TypingResponse struct {
	Status  int64  `json:"status"`
	Message string `json:"message"`
	Error   any    `json:"error"`
}

type MessageReadResponse struct {
	ChatGUID string `json:"chatGuid"`
	Read     bool   `json:"read"`
}

type ServerInfo struct {
	PrivateApi bool `json:"private_api"`
}

type ServerInfoResponse struct {
	Status  int64      `json:"status"`
	Message string     `json:"message"`
	Data    ServerInfo `json:"data"`
}

type ResolveIdentifierResponse struct {
	Status  int64  `json:"status"`
	Message string `json:"message"`
	Data    Handle `json:"data"`
}
