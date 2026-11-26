package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client fetches batches from the TokenRank cloud.
type Client struct {
	APIURL    string // normalised, no trailing slash
	Token     string // install token
	HTTP      *http.Client
	UserAgent string
}

// Next fetches the next due batch. It returns (nil, nil) on 204 No Content.
func (c *Client) Next(ctx context.Context) (*Envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIURL+"/v1/agent/batches/next", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch next batch: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil
	case http.StatusOK:
		var env Envelope
		dec := json.NewDecoder(io.LimitReader(resp.Body, 2*MaxPayloadBytes))
		if err := dec.Decode(&env); err != nil {
			return nil, fmt.Errorf("decode batch envelope: %w", err)
		}
		return &env, nil
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("fetch next batch: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
}
