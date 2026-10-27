package team

import "time"

type Message struct {
	ID        int64     `json:"id"`
	Team      string    `json:"team"`
	From      string    `json:"from"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
}

type PublishRequest struct {
	From string `json:"from"`
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
