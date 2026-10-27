package team

import "time"

const (
	VersionHeader     = "X-Team-MCP-Version"
	MemberTokenHeader = "X-Team-Member-Token"
)

type Message struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"sessionId"`
	From      string    `json:"from"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
}

type Member struct {
	Name       string    `json:"name"`
	MCPVersion string    `json:"mcpVersion"`
	JoinedAt   time.Time `json:"joinedAt"`
	LastSeen   time.Time `json:"lastSeen"`
}

type SessionInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	Members   []Member  `json:"members"`
	LatestID  int64     `json:"latestId"`
}

type CreateSessionRequest struct {
	Name      string `json:"name"`
	CreatedBy string `json:"createdBy"`
}

type JoinRequest struct {
	Name string `json:"name"`
}

type JoinResponse struct {
	Session     SessionInfo `json:"session"`
	JoinID      int64       `json:"joinMessageId"`
	MemberToken string      `json:"memberToken"`
}

type PublishRequest struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type PublishResponse struct {
	ID int64 `json:"id"`
}

type MessagesResponse struct {
	Messages []Message `json:"messages"`
	LatestID int64     `json:"latestId"`
	HasMore  bool      `json:"hasMore"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}
