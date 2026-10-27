package teamclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hudl/claude-teams/internal/team"
)

const maxResponseBytes = 8 << 20

type Client struct {
	baseURL    string
	token      string
	version    string
	httpClient *http.Client
}

func New(rawURL, token, version string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("url must be an absolute http(s) URL, got %q", rawURL)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("token is required; it is printed by team-server when it starts")
	}
	if version == "" {
		return nil, errors.New("client version is required")
	}
	return &Client{
		baseURL:    strings.TrimRight(parsed.String(), "/"),
		token:      token,
		version:    version,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) CreateSession(ctx context.Context, request team.CreateSessionRequest) (team.SessionInfo, error) {
	var response team.SessionInfo
	err := c.doJSON(ctx, http.MethodPost, "/sessions", request, &response)
	return response, err
}

func (c *Client) StopSession(ctx context.Context, sessionID string) (team.SessionInfo, error) {
	var response team.SessionInfo
	err := c.do(ctx, http.MethodDelete, sessionPath(sessionID), nil, &response)
	return response, err
}

func (c *Client) Join(ctx context.Context, sessionID, name string) (team.JoinResponse, error) {
	var response team.JoinResponse
	err := c.doJSON(ctx, http.MethodPost, sessionPath(sessionID)+"/members", team.JoinRequest{Name: name}, &response)
	return response, err
}

func (c *Client) Publish(ctx context.Context, sessionID string, request team.PublishRequest) (team.PublishResponse, error) {
	var response team.PublishResponse
	err := c.doJSON(ctx, http.MethodPost, sessionPath(sessionID)+"/messages", request, &response)
	return response, err
}

func (c *Client) Since(ctx context.Context, sessionID, member string, since int64, limit int) (team.MessagesResponse, error) {
	var response team.MessagesResponse
	query := url.Values{}
	query.Set("since", strconv.FormatInt(since, 10))
	query.Set("limit", strconv.Itoa(limit))
	query.Set("member", member)
	err := c.do(ctx, http.MethodGet, sessionPath(sessionID)+"/messages?"+query.Encode(), nil, &response)
	return response, err
}

func sessionPath(sessionID string) string {
	return "/sessions/" + url.PathEscape(strings.TrimSpace(sessionID))
}

func (c *Client) doJSON(ctx context.Context, method, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, bytes.NewReader(body), out)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("ngrok-skip-browser-warning", "true")
	request.Header.Set(team.VersionHeader, c.version)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if response.StatusCode >= 300 {
		var serverError team.ErrorResponse
		if json.Unmarshal(payload, &serverError) == nil && serverError.Error != "" {
			return fmt.Errorf("server returned %d: %s", response.StatusCode, serverError.Error)
		}
		return fmt.Errorf("server returned %d: %s", response.StatusCode, truncate(string(payload), 200))
	}

	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response (is the URL pointing at team-server?): %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
