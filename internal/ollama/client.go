package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client is a typed HTTP client for the Ollama REST API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient constructs a Client targeting the given base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// PS calls GET /api/ps and returns currently loaded models.
func (c *Client) PS(ctx context.Context) (*PSResponse, error) {
	return get[PSResponse](ctx, c, "/api/ps")
}

// Tags calls GET /api/tags and returns all locally pulled models.
func (c *Client) Tags(ctx context.Context) (*TagsResponse, error) {
	return get[TagsResponse](ctx, c, "/api/tags")
}

// Health calls GET / and returns nil if Ollama is reachable.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}

func get[T any](ctx context.Context, c *Client, path string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, path)
	}
	var result T
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}
