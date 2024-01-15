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
