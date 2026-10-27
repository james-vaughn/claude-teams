package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hudl/claude-teams/internal/team"
)

const (
	testToken   = "test-token"
	testVersion = "0.1.0"
)

type testServer struct {
	t       *testing.T
	baseURL string
	version string
}

func openTestStore(t *testing.T, path string, maxSessions int) *Store {
	t.Helper()
	store, err := OpenStore(context.Background(), path, maxSessions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newTestServer(t *testing.T, maxSessions int) *testServer {
	t.Helper()
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"), maxSessions)
	server := httptest.NewServer(NewHandler(store, testToken))
	t.Cleanup(server.Close)
	return &testServer{t: t, baseURL: server.URL, version: testVersion}
}

func (s *testServer) request(method, path, token string, payload any) (int, []byte) {
	s.t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			s.t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, s.baseURL+path, body)
	if err != nil {
		s.t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if s.version != "" {
		request.Header.Set(team.VersionHeader, s.version)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return response.StatusCode, raw
}

func (s *testServer) do(method, path string, payload, out any, wantStatus int) {
	s.t.Helper()
	status, raw := s.request(method, path, testToken, payload)
	if status != wantStatus {
		s.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, wantStatus, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			s.t.Fatal(err)
		}
	}
}

func (s *testServer) createSession(createdBy string) team.SessionInfo {
	s.t.Helper()
	var created team.SessionInfo
	s.do(http.MethodPost, "/sessions", team.CreateSessionRequest{Name: "demo", CreatedBy: createdBy}, &created, http.StatusCreated)
	return created
}

func (s *testServer) join(sessionID, name string) team.JoinResponse {
	s.t.Helper()
	var joined team.JoinResponse
	s.do(http.MethodPost, "/sessions/"+sessionID+"/members", team.JoinRequest{Name: name}, &joined, http.StatusOK)
	return joined
}

func (s *testServer) publish(sessionID, from, text string) int64 {
	s.t.Helper()
	var published team.PublishResponse
	s.do(http.MethodPost, "/sessions/"+sessionID+"/messages", team.PublishRequest{From: from, Kind: "finding", Text: text}, &published, http.StatusCreated)
	return published.ID
}

func (s *testServer) messages(sessionID, query string) team.MessagesResponse {
	s.t.Helper()
	var page team.MessagesResponse
	s.do(http.MethodGet, "/sessions/"+sessionID+"/messages?"+query, nil, &page, http.StatusOK)
	return page
}

func TestCreateSessionReturnsUUID(t *testing.T) {
	server := newTestServer(t, 10)
	created := server.createSession("alice")

	if !sessionIDPattern.MatchString(created.ID) {
		t.Fatalf("session id %q is not a UUID", created.ID)
	}
	if created.Name != "demo" || created.CreatedBy != "alice" || created.LatestID != 0 || len(created.Members) != 0 {
		t.Fatalf("unexpected session: %+v", created)
	}
	if other := server.createSession("alice"); other.ID == created.ID {
		t.Fatal("two sessions got the same id")
	}
}

func TestJoinPublishAndReceive(t *testing.T) {
	server := newTestServer(t, 10)
	sessionID := server.createSession("alice").ID

	joined := server.join(sessionID, "alice")
	if joined.JoinID != 1 || len(joined.Session.Members) != 1 || joined.Session.Members[0].Name != "alice" || joined.Session.Members[0].MCPVersion != testVersion {
		t.Fatalf("unexpected join: %+v", joined)
	}
	server.join(sessionID, "bob")

	if id := server.publish(sessionID, "alice", "first"); id != 3 {
		t.Fatalf("publish id = %d, want 3", id)
	}
	server.publish(sessionID, "bob", "second")

	page := server.messages(sessionID, "since=2&member=bob")
	if len(page.Messages) != 2 || page.Messages[0].Text != "first" || page.Messages[1].From != "bob" {
		t.Fatalf("unexpected messages: %+v", page.Messages)
	}
	if page.Messages[0].SessionID != sessionID || page.Messages[0].Kind != "finding" {
		t.Fatalf("message fields not preserved: %+v", page.Messages[0])
	}
	if page.LatestID != 4 || page.HasMore {
		t.Fatalf("latestId=%d hasMore=%v", page.LatestID, page.HasMore)
	}

	var info team.SessionInfo
	server.do(http.MethodGet, "/sessions/"+sessionID, nil, &info, http.StatusOK)
	if len(info.Members) != 2 || info.Members[0].Name != "alice" || info.Members[1].Name != "bob" {
		t.Fatalf("unexpected members: %+v", info.Members)
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	server := newTestServer(t, 10)
	first := server.createSession("alice").ID
	second := server.createSession("bob").ID
	server.join(first, "alice")
	server.join(second, "bob")

	server.publish(first, "alice", "only in first")

	if page := server.messages(second, ""); len(page.Messages) != 1 || page.Messages[0].Kind != "join" {
		t.Fatalf("second session saw messages from first: %+v", page.Messages)
	}
	if id := server.publish(second, "bob", "second's own"); id != 2 {
		t.Fatalf("second session id = %d, want 2", id)
	}

	status, _ := server.request(http.MethodPost, "/sessions/"+second+"/messages", testToken, team.PublishRequest{From: "alice", Text: "x"})
	if status != http.StatusForbidden {
		t.Fatalf("member of another session publishing: status %d, want 403", status)
	}
}

func TestStopSessionRemovesIt(t *testing.T) {
	server := newTestServer(t, 10)
	sessionID := server.createSession("alice").ID
	server.join(sessionID, "alice")
	server.publish(sessionID, "alice", "hello")

	var stopped team.SessionInfo
	server.do(http.MethodDelete, "/sessions/"+sessionID, nil, &stopped, http.StatusOK)
	if stopped.ID != sessionID || stopped.LatestID != 2 {
		t.Fatalf("unexpected stop result: %+v", stopped)
	}

	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/sessions/" + sessionID},
		{http.MethodGet, "/sessions/" + sessionID + "/messages"},
		{http.MethodDelete, "/sessions/" + sessionID},
	} {
		if status, _ := server.request(call.method, call.path, testToken, nil); status != http.StatusNotFound {
			t.Fatalf("%s %s after stop: status %d, want 404", call.method, call.path, status)
		}
	}
	if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/members", testToken, team.JoinRequest{Name: "bob"}); status != http.StatusNotFound {
		t.Fatalf("join after stop: status %d, want 404", status)
	}
}

func TestPublishRequiresMembership(t *testing.T) {
	server := newTestServer(t, 10)
	sessionID := server.createSession("alice").ID

	status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/messages", testToken, team.PublishRequest{From: "alice", Text: "x"})
	if status != http.StatusForbidden {
		t.Fatalf("status %d, want 403", status)
	}
}

func TestMessagesHonorsLimit(t *testing.T) {
	server := newTestServer(t, 10)
	sessionID := server.createSession("alice").ID
	server.join(sessionID, "alice")
	for range 4 {
		server.publish(sessionID, "alice", "x")
	}

	if page := server.messages(sessionID, "since=0&limit=2"); len(page.Messages) != 2 || !page.HasMore || page.Messages[1].ID != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}
	if page := server.messages(sessionID, "since=99"); len(page.Messages) != 0 || page.LatestID != 5 {
		t.Fatalf("since beyond latest: %+v", page)
	}
}

func TestEmptySessionReturnsEmptyArray(t *testing.T) {
	server := newTestServer(t, 10)
	sessionID := server.createSession("alice").ID

	status, raw := server.request(http.MethodGet, "/sessions/"+sessionID+"/messages", testToken, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"messages":[]`) {
		t.Fatalf("status %d body %s", status, raw)
	}
}

func TestUnknownAndInvalidSessionIDs(t *testing.T) {
	server := newTestServer(t, 10)

	if status, _ := server.request(http.MethodGet, "/sessions/00000000-0000-4000-8000-000000000000", testToken, nil); status != http.StatusNotFound {
		t.Fatalf("unknown session: status %d, want 404", status)
	}
	if status, _ := server.request(http.MethodGet, "/sessions/not-a-uuid", testToken, nil); status != http.StatusBadRequest {
		t.Fatalf("invalid session id: status %d, want 400", status)
	}
}

func TestValidation(t *testing.T) {
	server := newTestServer(t, 10)
	sessionID := server.createSession("alice").ID
	server.join(sessionID, "alice")

	publishCases := map[string]team.PublishRequest{
		"missing from":  {Text: "x"},
		"missing text":  {From: "alice"},
		"text too long": {From: "alice", Text: strings.Repeat("x", maxTextRunes+1)},
	}
	for name, request := range publishCases {
		t.Run(name, func(t *testing.T) {
			if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/messages", testToken, request); status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", status)
			}
		})
	}

	if status, _ := server.request(http.MethodPost, "/sessions", testToken, team.CreateSessionRequest{}); status != http.StatusBadRequest {
		t.Fatalf("create without createdBy: status %d, want 400", status)
	}
	if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/members", testToken, team.JoinRequest{}); status != http.StatusBadRequest {
		t.Fatalf("join without name: status %d, want 400", status)
	}
	for _, query := range []string{"since=-1", "since=abc", "limit=0", "limit=501"} {
		if status, _ := server.request(http.MethodGet, "/sessions/"+sessionID+"/messages?"+query, testToken, nil); status != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", query, status)
		}
	}
}

func TestSessionLimit(t *testing.T) {
	server := newTestServer(t, 1)
	sessionID := server.createSession("alice").ID

	if status, _ := server.request(http.MethodPost, "/sessions", testToken, team.CreateSessionRequest{CreatedBy: "bob"}); status != http.StatusServiceUnavailable {
		t.Fatalf("over limit: status %d, want 503", status)
	}
	server.do(http.MethodDelete, "/sessions/"+sessionID, nil, nil, http.StatusOK)
	server.createSession("bob")
}

func TestTokenAlwaysRequired(t *testing.T) {
	server := newTestServer(t, 10)
	sessionID := server.createSession("alice").ID

	for _, token := range []string{"", "wrong"} {
		for _, call := range []struct{ method, path string }{
			{http.MethodPost, "/sessions"},
			{http.MethodGet, "/sessions/" + sessionID},
			{http.MethodDelete, "/sessions/" + sessionID},
			{http.MethodPost, "/sessions/" + sessionID + "/members"},
			{http.MethodPost, "/sessions/" + sessionID + "/messages"},
			{http.MethodGet, "/sessions/" + sessionID + "/messages"},
		} {
			if status, _ := server.request(call.method, call.path, token, nil); status != http.StatusUnauthorized {
				t.Fatalf("token %q %s %s: status %d, want 401", token, call.method, call.path, status)
			}
		}
	}

	if status, _ := server.request(http.MethodGet, "/healthz", "", nil); status != http.StatusOK {
		t.Fatalf("healthz: status %d, want 200", status)
	}
}

func TestVersionAlwaysRequired(t *testing.T) {
	server := newTestServer(t, 10)
	sessionID := server.createSession("alice").ID

	for _, version := range []string{"", "latest", "1.2", "0.1.0 beta", "1.0.0-" + strings.Repeat("a", 40)} {
		server.version = version
		for _, call := range []struct{ method, path string }{
			{http.MethodPost, "/sessions"},
			{http.MethodGet, "/sessions/" + sessionID},
			{http.MethodDelete, "/sessions/" + sessionID},
			{http.MethodPost, "/sessions/" + sessionID + "/members"},
			{http.MethodPost, "/sessions/" + sessionID + "/messages"},
			{http.MethodGet, "/sessions/" + sessionID + "/messages"},
		} {
			if status, _ := server.request(call.method, call.path, testToken, nil); status != http.StatusBadRequest {
				t.Fatalf("version %q %s %s: status %d, want 400", version, call.method, call.path, status)
			}
		}
	}

	server.version = ""
	if status, _ := server.request(http.MethodGet, "/sessions/"+sessionID, "wrong", nil); status != http.StatusUnauthorized {
		t.Fatalf("wrong token without version: status %d, want 401", status)
	}
	if status, _ := server.request(http.MethodGet, "/healthz", "", nil); status != http.StatusOK {
		t.Fatalf("healthz without version: status %d, want 200", status)
	}

	server.version = "0.2.0-rc.1"
	if joined := server.join(sessionID, "bob"); joined.Session.Members[0].MCPVersion != "0.2.0-rc.1" {
		t.Fatalf("unexpected members: %+v", joined.Session.Members)
	}
	server.version = "0.3.0"
	if joined := server.join(sessionID, "bob"); joined.Session.Members[0].MCPVersion != "0.3.0" {
		t.Fatalf("rejoin did not update version: %+v", joined.Session.Members)
	}
}

func TestNewHandlerRejectsEmptyToken(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for empty token")
		}
	}()
	NewHandler(openTestStore(t, filepath.Join(t.TempDir(), "test.db"), 1), "")
}
