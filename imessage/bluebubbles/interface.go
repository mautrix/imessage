package bluebubbles

type PageMetadata struct {
	Count  int64 `json:"count"`
	Total  int64 `json:"total"`
	Offset int64 `json:"offset"`
	Limit  int64 `json:"limit"`
}

type QuerySort string

const (
	QuerySortLastMessage QuerySort = "lastmessage"
)

type ChatQueryRequest struct {
	// TODO Other Fields
	Limit  int64           `json:"limit"`
	Offset int64           `json:"offset"`
	With   []ChatQueryWith `json:"with"`
	Sort   QuerySort       `json:"sort"`
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
	GUID           string        `json:"guid"`
	ChatIdentifier string        `json:"chatIdentifier"`
	GroupID        string        `json:"groupId,omitempty"`
	DisplayName    string        `json:"displayName"`
	Partipants     []Participant `json:"participants"`
	LastMessage    *Message      `json:"lastMessage,omitempty"`
}

type Participant struct {
	Address string `json:"address"`
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
	Avatar       string        `json:"avatar,omitempty"`
	SourceType   string        `json:"sourceType,omitempty"`
	ID           string        `json:"id,omitempty"`
}

type PhoneNumber struct {
	Address string      `json:"address,omitempty"`
	ID      interface{} `json:"id,omitempty"`
}

type Email struct {
	Address string      `json:"address,omitempty"`
	ID      interface{} `json:"id,omitempty"`
}

type TypingNotification struct {
	Display bool   `json:"display"`
	GUID    string `json:"guid"`
}

type Message struct {
	AssociatedMessageGuid string        `json:"associatedMessageGuid,omitempty"`
	AssociatedMessageType string        `json:"associatedMessageType,omitempty"`
	Attachments           []Attachment  `json:"attachments,omitempty"`
	AttributedBody        []interface{} `json:"attributedBody,omitempty"`
	BalloonBundleId       interface{}   `json:"balloonBundleId,omitempty"`
	Chats                 []Chat        `json:"chats,omitempty"`
	DateCreated           int64         `json:"dateCreated,omitempty"`
	DateDelivered         int64         `json:"dateDelivered,omitempty"`
	DateEdited            int64         `json:"dateEdited,omitempty"`
	DateRead              int64         `json:"dateRead,omitempty"`
	DateRetracted         int64         `json:"dateRetracted,omitempty"`
	Error                 int           `json:"error,omitempty"`
	ExpressiveSendStyleId interface{}   `json:"expressiveSendStyleId,omitempty"`
	GroupActionType       int           `json:"groupActionType,omitempty"`
	GroupTitle            string        `json:"groupTitle,omitempty"`
	GUID                  string        `json:"guid,omitempty"`
	Handle                Handle        `json:"handle,omitempty"`
	HandleId              int           `json:"handleId,omitempty"`
	HasDdResults          bool          `json:"hasDdResults,omitempty"`
	HasPayloadData        bool          `json:"hasPayloadData,omitempty"`
	IsAudioMessage        bool          `json:"isAudioMessage,omitempty"`
	IsArchived            bool          `json:"isArchived,omitempty"`
	IsFromMe              bool          `json:"isFromMe,omitempty"`
	ItemType              int           `json:"itemType,omitempty"`
	MessageSummaryInfo    interface{}   `json:"messageSummaryInfo,omitempty"`
	OriginalROWID         int           `json:"originalROWID,omitempty"`
	OtherHandle           int           `json:"otherHandle,omitempty"`
	PartCount             int           `json:"partCount,omitempty"`
	PayloadData           interface{}   `json:"payloadData,omitempty"`
	Subject               string        `json:"subject,omitempty"`
	Text                  string        `json:"text,omitempty"`
	ThreadOriginatorGuid  string        `json:"threadOriginatorGuid,omitempty"`
}

type Attachment struct {
	OriginalRowID  int         `json:"originalROWID,omitempty"`
	GUID           string      `json:"guid,omitempty"`
	UTI            string      `json:"uti,omitempty"`
	MimeType       string      `json:"mimeType,omitempty"`
	TransferName   string      `json:"transferName,omitempty"`
	TotalBytes     int64       `json:"totalBytes,omitempty"`
	TransferState  int         `json:"transferState,omitempty"`
	IsOutgoing     bool        `json:"isOutgoing,omitempty"`
	HideAttachment bool        `json:"hideAttachment,omitempty"`
	IsSticker      bool        `json:"isSticker,omitempty"`
	OriginalGUID   string      `json:"originalGuid,omitempty"`
	HasLivePhoto   bool        `json:"hasLivePhoto,omitempty"`
	Height         int64       `json:"height,omitempty"`
	Width          int64       `json:"width,omitempty"`
	Metadata       interface{} `json:"metadata,omitempty"`
}

type MessageResponse struct {
	Status  int64       `json:"status"`
	Message string      `json:"message"`
	Data    Message     `json:"data"`
	Error   interface{} `json:"error,omitempty"`
}

type Handle struct {
	Address           string      `json:"address,omitempty"`
	Country           string      `json:"country,omitempty"`
	OriginalROWID     int         `json:"originalROWID,omitempty"`
	Service           string      `json:"service,omitempty"`
	UncanonicalizedId interface{} `json:"uncanonicalizedId,omitempty"`
}

type SendTextRequest struct {
	ChatGUID            string `json:"chatGuid"`
	Method              string `json:"method"`
	Message             string `json:"message"`
	SelectedMessageGuid string `json:"selectedMessageGuid"`
	PartIndex           int    `json:"partIndex"`
}

type SendTextResponse struct {
	Status  int64   `json:"status"`
	Message string  `json:"message"`
	Data    Message `json:"data,omitempty"`
}

type ReadReceiptResponse struct {
	Status  int64       `json:"status"`
	Message string      `json:"message"`
	Error   interface{} `json:"error"`
}

type TypingResponse struct {
	Status  int64       `json:"status"`
	Message string      `json:"message"`
	Error   interface{} `json:"error"`
}

type MessageReadResponse struct {
	ChatGUID string `json:"chatGuid"`
	Read     bool   `json:"read"`
}
