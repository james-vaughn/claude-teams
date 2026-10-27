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

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := OpenStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	server := httptest.NewServer(NewHandler(store, testToken))
	t.Cleanup(server.Close)
	return &testServer{t: t, baseURL: server.URL, version: testVersion}
}

func (s *testServer) request(method, path, token, memberToken string, payload any) (int, []byte) {
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
	if memberToken != "" {
		request.Header.Set(team.MemberTokenHeader, memberToken)
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

func (s *testServer) do(method, path, memberToken string, payload, out any, wantStatus int) {
	s.t.Helper()
	status, raw := s.request(method, path, testToken, memberToken, payload)
	if status != wantStatus {
		s.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, wantStatus, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			s.t.Fatal(err)
		}
	}
}

func (s *testServer) createSession(createdBy string) team.JoinResponse {
	s.t.Helper()
	var created team.JoinResponse
	s.do(http.MethodPost, "/sessions", "", team.CreateSessionRequest{Name: "demo", CreatedBy: createdBy}, &created, http.StatusCreated)
	return created
}

func (s *testServer) join(sessionID, name string) team.JoinResponse {
	s.t.Helper()
	var joined team.JoinResponse
	s.do(http.MethodPost, "/sessions/"+sessionID+"/members", "", team.JoinRequest{Name: name}, &joined, http.StatusOK)
	return joined
}

func (s *testServer) publish(sessionID, memberToken, text string) int64 {
	s.t.Helper()
	var published team.PublishResponse
	s.do(http.MethodPost, "/sessions/"+sessionID+"/messages", memberToken, team.PublishRequest{Kind: "finding", Text: text}, &published, http.StatusCreated)
	return published.ID
}

func (s *testServer) messages(sessionID, memberToken, query string) team.MessagesResponse {
	s.t.Helper()
	var page team.MessagesResponse
	s.do(http.MethodGet, "/sessions/"+sessionID+"/messages?"+query, memberToken, nil, &page, http.StatusOK)
	return page
}

func TestCreateSessionJoinsCreator(t *testing.T) {
	server := newTestServer(t)
	created := server.createSession("alice")

	if !sessionIDPattern.MatchString(created.Session.ID) {
		t.Fatalf("session id %q is not a UUID", created.Session.ID)
	}
	if created.Session.Name != "demo" || created.Session.CreatedBy != "alice" || created.Session.LatestID != 1 || created.JoinID != 1 {
		t.Fatalf("unexpected session: %+v", created)
	}
	if len(created.Session.Members) != 1 || created.Session.Members[0].Name != "alice" || created.Session.Members[0].MCPVersion != testVersion {
		t.Fatalf("creator is not a member: %+v", created.Session.Members)
	}
	if len(created.MemberToken) != 64 {
		t.Fatalf("member token %q is not 32 hex-encoded bytes", created.MemberToken)
	}
	if other := server.createSession("alice"); other.Session.ID == created.Session.ID || other.MemberToken == created.MemberToken {
		t.Fatal("two sessions got the same id or member token")
	}
}

func TestJoinPublishAndReceive(t *testing.T) {
	server := newTestServer(t)
	alice := server.createSession("alice")
	sessionID := alice.Session.ID

	bob := server.join(sessionID, "bob")
	if bob.JoinID != 2 || len(bob.Session.Members) != 2 || bob.Session.Members[1].Name != "bob" || bob.MemberToken == alice.MemberToken {
		t.Fatalf("unexpected join: %+v", bob)
	}

	if id := server.publish(sessionID, alice.MemberToken, "first"); id != 3 {
		t.Fatalf("publish id = %d, want 3", id)
	}
	server.publish(sessionID, bob.MemberToken, "second")

	page := server.messages(sessionID, bob.MemberToken, "since=2")
	if len(page.Messages) != 2 || page.Messages[0].Text != "first" || page.Messages[0].From != "alice" || page.Messages[1].From != "bob" {
		t.Fatalf("unexpected messages: %+v", page.Messages)
	}
	if page.Messages[0].SessionID != sessionID || page.Messages[0].Kind != "finding" {
		t.Fatalf("message fields not preserved: %+v", page.Messages[0])
	}
	if page.LatestID != 4 || page.HasMore {
		t.Fatalf("latestId=%d hasMore=%v", page.LatestID, page.HasMore)
	}

	var info team.SessionInfo
	server.do(http.MethodGet, "/sessions/"+sessionID, alice.MemberToken, nil, &info, http.StatusOK)
	if len(info.Members) != 2 || info.Members[0].Name != "alice" || info.Members[1].Name != "bob" {
		t.Fatalf("unexpected members: %+v", info.Members)
	}
}

func TestPublishTakesSenderFromMemberToken(t *testing.T) {
	server := newTestServer(t)
	alice := server.createSession("alice")
	sessionID := alice.Session.ID
	bob := server.join(sessionID, "bob")

	spoofed := map[string]string{"from": "alice", "text": "pretending to be alice"}
	if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/messages", testToken, bob.MemberToken, spoofed); status != http.StatusBadRequest {
		t.Fatalf("publish with a from field: status %d, want 400", status)
	}

	server.publish(sessionID, bob.MemberToken, "from bob")
	page := server.messages(sessionID, alice.MemberToken, "since=2")
	if len(page.Messages) != 1 || page.Messages[0].From != "bob" {
		t.Fatalf("unexpected messages: %+v", page.Messages)
	}
}

func TestNameCannotBeTakenOver(t *testing.T) {
	server := newTestServer(t)
	alice := server.createSession("alice")
	sessionID := alice.Session.ID

	if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/members", testToken, "", team.JoinRequest{Name: "alice"}); status != http.StatusConflict {
		t.Fatalf("join with a taken name: status %d, want 409", status)
	}
	bob := server.join(sessionID, "bob")
	if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/members", testToken, bob.MemberToken, team.JoinRequest{Name: "alice"}); status != http.StatusConflict {
		t.Fatalf("join with a taken name and another member's token: status %d, want 409", status)
	}

	var rejoined team.JoinResponse
	server.do(http.MethodPost, "/sessions/"+sessionID+"/members", alice.MemberToken, team.JoinRequest{Name: "alice"}, &rejoined, http.StatusOK)
	if rejoined.MemberToken == "" || rejoined.MemberToken == alice.MemberToken {
		t.Fatalf("rejoin did not issue a new member token: %+v", rejoined)
	}
	if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/messages", testToken, alice.MemberToken, team.PublishRequest{Text: "x"}); status != http.StatusForbidden {
		t.Fatalf("publish with the replaced member token: status %d, want 403", status)
	}
	server.publish(sessionID, rejoined.MemberToken, "after rejoin")
}

func TestReadingAndStoppingRequireMembership(t *testing.T) {
	server := newTestServer(t)
	sessionID := server.createSession("alice").Session.ID
	outsider := server.createSession("mallory")

	for _, memberToken := range []string{"", "not-a-member-token", outsider.MemberToken} {
		for _, call := range []struct{ method, path string }{
			{http.MethodGet, "/sessions/" + sessionID},
			{http.MethodDelete, "/sessions/" + sessionID},
			{http.MethodGet, "/sessions/" + sessionID + "/messages"},
		} {
			if status, _ := server.request(call.method, call.path, testToken, memberToken, nil); status != http.StatusForbidden {
				t.Fatalf("member token %q %s %s: status %d, want 403", memberToken, call.method, call.path, status)
			}
		}
		if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/messages", testToken, memberToken, team.PublishRequest{Text: "x"}); status != http.StatusForbidden {
			t.Fatalf("member token %q publish: status %d, want 403", memberToken, status)
		}
	}
}

func TestAnyMemberCanStopSession(t *testing.T) {
	server := newTestServer(t)
	alice := server.createSession("alice")
	sessionID := alice.Session.ID
	bob := server.join(sessionID, "bob")
	server.publish(sessionID, alice.MemberToken, "hello")

	var stopped team.SessionInfo
	server.do(http.MethodDelete, "/sessions/"+sessionID, bob.MemberToken, nil, &stopped, http.StatusOK)
	if stopped.ID != sessionID || stopped.LatestID != 3 {
		t.Fatalf("unexpected stop result: %+v", stopped)
	}

	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/sessions/" + sessionID},
		{http.MethodGet, "/sessions/" + sessionID + "/messages"},
		{http.MethodDelete, "/sessions/" + sessionID},
	} {
		if status, _ := server.request(call.method, call.path, testToken, alice.MemberToken, nil); status != http.StatusNotFound {
			t.Fatalf("%s %s after stop: status %d, want 404", call.method, call.path, status)
		}
	}
	if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/members", testToken, "", team.JoinRequest{Name: "carol"}); status != http.StatusNotFound {
		t.Fatalf("join after stop: status %d, want 404", status)
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	server := newTestServer(t)
	alice := server.createSession("alice")
	bob := server.createSession("bob")

	server.publish(alice.Session.ID, alice.MemberToken, "only in first")

	if page := server.messages(bob.Session.ID, bob.MemberToken, ""); len(page.Messages) != 1 || page.Messages[0].Kind != "join" {
		t.Fatalf("second session saw messages from first: %+v", page.Messages)
	}
	if id := server.publish(bob.Session.ID, bob.MemberToken, "second's own"); id != 2 {
		t.Fatalf("second session id = %d, want 2", id)
	}
}

func TestMessagesHonorsLimit(t *testing.T) {
	server := newTestServer(t)
	alice := server.createSession("alice")
	sessionID := alice.Session.ID
	for range 4 {
		server.publish(sessionID, alice.MemberToken, "x")
	}

	if page := server.messages(sessionID, alice.MemberToken, "since=0&limit=2"); len(page.Messages) != 2 || !page.HasMore || page.Messages[1].ID != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}
	if page := server.messages(sessionID, alice.MemberToken, "since=99"); len(page.Messages) != 0 || page.LatestID != 5 {
		t.Fatalf("since beyond latest: %+v", page)
	}
}

func TestEmptyPageReturnsEmptyArray(t *testing.T) {
	server := newTestServer(t)
	alice := server.createSession("alice")

	status, raw := server.request(http.MethodGet, "/sessions/"+alice.Session.ID+"/messages?since=1", testToken, alice.MemberToken, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"messages":[]`) {
		t.Fatalf("status %d body %s", status, raw)
	}
}

func TestUnknownAndInvalidSessionIDs(t *testing.T) {
	server := newTestServer(t)

	if status, _ := server.request(http.MethodGet, "/sessions/00000000-0000-4000-8000-000000000000", testToken, "", nil); status != http.StatusNotFound {
		t.Fatalf("unknown session: status %d, want 404", status)
	}
	if status, _ := server.request(http.MethodGet, "/sessions/not-a-uuid", testToken, "", nil); status != http.StatusBadRequest {
		t.Fatalf("invalid session id: status %d, want 400", status)
	}
}

func TestValidation(t *testing.T) {
	server := newTestServer(t)
	alice := server.createSession("alice")
	sessionID := alice.Session.ID

	publishCases := map[string]team.PublishRequest{
		"missing text":  {},
		"text too long": {Text: strings.Repeat("x", maxTextRunes+1)},
		"kind too long": {Kind: strings.Repeat("k", maxKindRunes+1), Text: "x"},
	}
	for name, request := range publishCases {
		t.Run(name, func(t *testing.T) {
			if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/messages", testToken, alice.MemberToken, request); status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", status)
			}
		})
	}

	if status, _ := server.request(http.MethodPost, "/sessions", testToken, "", team.CreateSessionRequest{}); status != http.StatusBadRequest {
		t.Fatalf("create without createdBy: status %d, want 400", status)
	}
	if status, _ := server.request(http.MethodPost, "/sessions/"+sessionID+"/members", testToken, "", team.JoinRequest{}); status != http.StatusBadRequest {
		t.Fatalf("join without name: status %d, want 400", status)
	}
	for _, query := range []string{"since=-1", "since=abc", "limit=0", "limit=501"} {
		if status, _ := server.request(http.MethodGet, "/sessions/"+sessionID+"/messages?"+query, testToken, alice.MemberToken, nil); status != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", query, status)
		}
	}
}

func TestTokenAlwaysRequired(t *testing.T) {
	server := newTestServer(t)
	alice := server.createSession("alice")
	sessionID := alice.Session.ID

	for _, token := range []string{"", "wrong"} {
		for _, call := range []struct{ method, path string }{
			{http.MethodPost, "/sessions"},
			{http.MethodGet, "/sessions/" + sessionID},
			{http.MethodDelete, "/sessions/" + sessionID},
			{http.MethodPost, "/sessions/" + sessionID + "/members"},
			{http.MethodPost, "/sessions/" + sessionID + "/messages"},
			{http.MethodGet, "/sessions/" + sessionID + "/messages"},
		} {
			if status, _ := server.request(call.method, call.path, token, alice.MemberToken, nil); status != http.StatusUnauthorized {
				t.Fatalf("token %q %s %s: status %d, want 401", token, call.method, call.path, status)
			}
		}
	}

	if status, _ := server.request(http.MethodGet, "/healthz", "", "", nil); status != http.StatusOK {
		t.Fatalf("healthz: status %d, want 200", status)
	}
}

func TestVersionAlwaysRequired(t *testing.T) {
	server := newTestServer(t)
	alice := server.createSession("alice")
	sessionID := alice.Session.ID

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
			if status, _ := server.request(call.method, call.path, testToken, alice.MemberToken, nil); status != http.StatusBadRequest {
				t.Fatalf("version %q %s %s: status %d, want 400", version, call.method, call.path, status)
			}
		}
	}

	server.version = ""
	if status, _ := server.request(http.MethodGet, "/sessions/"+sessionID, "wrong", alice.MemberToken, nil); status != http.StatusUnauthorized {
		t.Fatalf("wrong token without version: status %d, want 401", status)
	}
	if status, _ := server.request(http.MethodGet, "/healthz", "", "", nil); status != http.StatusOK {
		t.Fatalf("healthz without version: status %d, want 200", status)
	}

	server.version = "0.2.0-rc.1"
	bob := server.join(sessionID, "bob")
	if bob.Session.Members[1].MCPVersion != "0.2.0-rc.1" {
		t.Fatalf("unexpected members: %+v", bob.Session.Members)
	}
	server.version = "0.3.0"
	var rejoined team.JoinResponse
	server.do(http.MethodPost, "/sessions/"+sessionID+"/members", bob.MemberToken, team.JoinRequest{Name: "bob"}, &rejoined, http.StatusOK)
	if rejoined.Session.Members[1].MCPVersion != "0.3.0" {
		t.Fatalf("rejoin did not update version: %+v", rejoined.Session.Members)
	}
}

func TestNewHandlerRejectsEmptyToken(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for empty token")
		}
	}()
	NewHandler(openTestStore(t, filepath.Join(t.TempDir(), "test.db")), "")
}
