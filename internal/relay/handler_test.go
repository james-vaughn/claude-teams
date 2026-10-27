package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hudl/claude-teams/internal/team"
)

func newTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(NewHandler(NewStore(), token))
	t.Cleanup(server.Close)
	return server
}

func publish(t *testing.T, baseURL, teamName, token string, request team.PublishRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest, err := http.NewRequest(http.MethodPost, baseURL+"/teams/"+teamName+"/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func fetch(t *testing.T, url string) (int, team.MessagesResponse) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var page team.MessagesResponse
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, page
}

func TestPublishAssignsIncrementingIDsPerTeam(t *testing.T) {
	server := newTestServer(t, "")

	for want := int64(1); want <= 3; want++ {
		response := publish(t, server.URL, "alpha", "", team.PublishRequest{From: "a", Text: "hello"})
		var published team.PublishResponse
		if err := json.NewDecoder(response.Body).Decode(&published); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusCreated || published.ID != want {
			t.Fatalf("got status %d id %d, want 201 id %d", response.StatusCode, published.ID, want)
		}
	}

	response := publish(t, server.URL, "beta", "", team.PublishRequest{From: "b", Text: "hi"})
	var published team.PublishResponse
	if err := json.NewDecoder(response.Body).Decode(&published); err != nil {
		t.Fatal(err)
	}
	if published.ID != 1 {
		t.Fatalf("beta first id = %d, want 1", published.ID)
	}
}

func TestMessagesSinceReturnsOnlyNewerMessages(t *testing.T) {
	server := newTestServer(t, "")
	for _, text := range []string{"one", "two", "three"} {
		publish(t, server.URL, "alpha", "", team.PublishRequest{From: "a", Kind: "finding", Text: text})
	}

	status, page := fetch(t, server.URL+"/teams/alpha/messages?since=1")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(page.Messages) != 2 || page.Messages[0].Text != "two" || page.Messages[1].ID != 3 {
		t.Fatalf("unexpected messages: %+v", page.Messages)
	}
	if page.LatestID != 3 || page.HasMore {
		t.Fatalf("latestId=%d hasMore=%v", page.LatestID, page.HasMore)
	}
	if page.Messages[0].Kind != "finding" || page.Messages[0].From != "a" {
		t.Fatalf("message fields not preserved: %+v", page.Messages[0])
	}
}

func TestMessagesSinceHonorsLimit(t *testing.T) {
	server := newTestServer(t, "")
	for range 5 {
		publish(t, server.URL, "alpha", "", team.PublishRequest{From: "a", Text: "x"})
	}

	_, page := fetch(t, server.URL+"/teams/alpha/messages?since=0&limit=2")
	if len(page.Messages) != 2 || !page.HasMore || page.Messages[1].ID != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}

	_, page = fetch(t, server.URL+"/teams/alpha/messages?since=99")
	if len(page.Messages) != 0 || page.LatestID != 5 {
		t.Fatalf("since beyond latest: %+v", page)
	}
}

func TestEmptyTeamReturnsEmptyArray(t *testing.T) {
	server := newTestServer(t, "")
	response, err := http.Get(server.URL + "/teams/nobody/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var raw bytes.Buffer
	raw.ReadFrom(response.Body)
	if !strings.Contains(raw.String(), `"messages":[]`) {
		t.Fatalf("expected empty array, got %s", raw.String())
	}
}

func TestPublishValidation(t *testing.T) {
	server := newTestServer(t, "")
	cases := map[string]team.PublishRequest{
		"missing from":  {Text: "x"},
		"missing text":  {From: "a"},
		"text too long": {From: "a", Text: strings.Repeat("x", maxTextRunes+1)},
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			if response := publish(t, server.URL, "alpha", "", request); response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.StatusCode)
			}
		})
	}

	if response := publish(t, server.URL, "bad.team", "", team.PublishRequest{From: "a", Text: "x"}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid team status = %d, want 400", response.StatusCode)
	}
}

func TestInvalidQueryParams(t *testing.T) {
	server := newTestServer(t, "")
	for _, query := range []string{"since=-1", "since=abc", "limit=0", "limit=501"} {
		if status, _ := fetch(t, server.URL+"/teams/alpha/messages?"+query); status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", query, status)
		}
	}
}

func TestTokenRequiredWhenConfigured(t *testing.T) {
	server := newTestServer(t, "secret")

	if response := publish(t, server.URL, "alpha", "", team.PublishRequest{From: "a", Text: "x"}); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", response.StatusCode)
	}
	if response := publish(t, server.URL, "alpha", "wrong", team.PublishRequest{From: "a", Text: "x"}); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", response.StatusCode)
	}
	if response := publish(t, server.URL, "alpha", "secret", team.PublishRequest{From: "a", Text: "x"}); response.StatusCode != http.StatusCreated {
		t.Fatalf("valid token status = %d, want 201", response.StatusCode)
	}
	if status, _ := fetch(t, server.URL+"/teams/alpha/messages"); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read status = %d, want 401", status)
	}
	if status, _ := fetch(t, server.URL+"/healthz"); status != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", status)
	}
}
