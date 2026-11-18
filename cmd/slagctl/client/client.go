package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client is a thin HTTP client for the slagworks global API.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// Config holds client configuration.
type Config struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client // nil uses a default with 30s timeout
}

// APIError carries the HTTP status and message from a failed request.
type APIError struct {
	StatusCode int
	Message    string
}

// Error renders message-first: the server's explanation is what the
// operator acts on; the status code is diagnostic trivia that used to
// shout from the front of every error line.
func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the API server returned HTTP %d with no detail", e.StatusCode)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.StatusCode)
}

// New creates a new Client.
func New(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	return &Client{baseURL: baseURL, token: cfg.Token, httpClient: hc}
}

// InsecureWarning returns a non-empty string if the base URL uses http://
// and the host is not localhost or 127.0.0.1.
func (c *Client) InsecureWarning() string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" || h == "127.0.0.1" {
		return ""
	}
	return fmt.Sprintf("connecting to %s over http (unencrypted) — consider using https", c.baseURL)
}

// Get performs a GET request and JSON-decodes the response into out.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	fullURL := c.baseURL + path
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	return c.do(req, out)
}

// Post performs a POST request with a JSON body and decodes the response into out (may be nil).
func (c *Client) Post(ctx context.Context, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, r)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

// Put performs a PUT request with a JSON body and decodes the response into out (may be nil).
func (c *Client) Put(ctx context.Context, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, r)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

// Patch performs a PATCH request with a JSON body and decodes the response into out (may be nil).
func (c *Client) Patch(ctx context.Context, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+path, r)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

// Delete performs a DELETE request.
func (c *Client) Delete(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	return c.do(req, nil)
}

func (c *Client) setHeaders(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func (c *Client) do(req *http.Request, out any) error {
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.humanizeTransportErr(req, err, time.Since(start))
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(b))
		// Try to extract "message" field from JSON error body
		var errBody struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &errBody) == nil && errBody.Message != "" {
			msg = errBody.Message
		}
		return &APIError{StatusCode: resp.StatusCode, Message: msg}
	}

	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// humanizeTransportErr rewrites Go's transport-level error prose
// ("context deadline exceeded (Client.Timeout exceeded while awaiting
// headers)", full URL included twice by the caller chain) into a
// message that says what was being asked of which server, how long we
// waited, and what to do about it. The raw error is preserved via %w
// for errors.Is/As callers.
func (c *Client) humanizeTransportErr(req *http.Request, err error, waited time.Duration) error {
	target := fmt.Sprintf("%s %s on %s", req.Method, req.URL.Path, req.URL.Host)
	switch {
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return fmt.Errorf(
			"the API server did not respond within %s (%s).\n"+
				"The server accepted the connection but never sent a response — it is likely\n"+
				"stuck on slow upstream work (NetBox queries, AWS secrets). The request may\n"+
				"still complete server-side; check before retrying a mutation:\n"+
				"  slagctl operation list --machine <machine>   # did the op get created?\n"+
				"  kubectl logs -n slagworks deploy/slagworks   # what was the server doing?\n"+
				"underlying error: %w",
			waited.Round(time.Second), target, err)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("request interrupted (%s): %w", target, err)
	default:
		return fmt.Errorf("cannot reach the API server (%s): %w", target, err)
	}
}
