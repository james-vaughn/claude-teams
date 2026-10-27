package teamclient

import (
	"bytes"
	"context"
	"encoding/json"
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
	team       string
	token      string
	httpClient *http.Client
}

func New(rawURL, teamName, token string) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("url must be an absolute http(s) URL, got %q", rawURL)
	}
	return &Client{
		baseURL:    strings.TrimRight(parsed.String(), "/"),
		team:       teamName,
		token:      token,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *Client) BaseURL() string {
	return c.baseURL
}

func (c *Client) Publish(ctx context.Context, request team.PublishRequest) (team.PublishResponse, error) {
	var response team.PublishResponse
	body, err := json.Marshal(request)
	if err != nil {
		return response, err
	}
	err = c.do(ctx, http.MethodPost, c.messagesPath(), bytes.NewReader(body), &response)
	return response, err
}

func (c *Client) Since(ctx context.Context, since int64, limit int) (team.MessagesResponse, error) {
	var response team.MessagesResponse
	query := url.Values{}
	query.Set("since", strconv.FormatInt(since, 10))
	query.Set("limit", strconv.Itoa(limit))
	err := c.do(ctx, http.MethodGet, c.messagesPath()+"?"+query.Encode(), nil, &response)
	return response, err
}

func (c *Client) messagesPath() string {
	return "/teams/" + url.PathEscape(c.team) + "/messages"
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("ngrok-skip-browser-warning", "true")
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
