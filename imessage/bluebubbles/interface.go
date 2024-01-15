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

type Chat struct {
	// TODO How to get timestamp
	GUID           string `json:"guid"`
	ChatIdentifier string `json:"chatIdentifier"`
	GroupId        string `json:"groupId"`
}
