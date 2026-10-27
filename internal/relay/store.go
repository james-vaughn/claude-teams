package relay

import (
	"sync"
	"time"

	"github.com/hudl/claude-teams/internal/team"
)

type Store struct {
	mu    sync.RWMutex
	teams map[string][]team.Message
	now   func() time.Time
}

func NewStore() *Store {
	return &Store{teams: map[string][]team.Message{}, now: time.Now}
}

func (s *Store) Append(teamName, from, kind, text string) team.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	messages := s.teams[teamName]
	message := team.Message{
		ID:        int64(len(messages)) + 1,
		Team:      teamName,
		From:      from,
		Kind:      kind,
		Text:      text,
		CreatedAt: s.now().UTC(),
	}
	s.teams[teamName] = append(messages, message)
	return message
}

func (s *Store) Since(teamName string, since int64, limit int) team.MessagesResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	messages := s.teams[teamName]
	latestID := int64(len(messages))
	start := min(max(since, 0), latestID)
	end := min(start+int64(limit), latestID)

	page := make([]team.Message, end-start)
	copy(page, messages[start:end])

	return team.MessagesResponse{
		Messages: page,
		LatestID: latestID,
		HasMore:  end < latestID,
	}
}
