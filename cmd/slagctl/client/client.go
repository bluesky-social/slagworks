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
	"sync"
	"time"
)

// Client is a thin HTTP client for the slagworks global API.
type Client struct {
	baseURL    string
	bearer     func(context.Context) (string, error)
	httpClient *http.Client
	refresh    func(ctx context.Context, rejected string) (string, error)

	mu sync.Mutex
	// refreshOff is set once a refresh failed or the API refused the token
	// it returned too. The API is then not refusing a token that a refresh
	// replaces (a dead session, or a server that rejects valid tokens), and
	// asking again would only call the identity provider on every request:
	// a watch loop would rotate the refresh token on every tick.
	refreshOff bool
	// refreshErr is the refresher's error when that is why refreshing
	// stopped. Every later 401 carries it (APIError.Unwrap).
	refreshErr error
}

// Config holds client configuration.
type Config struct {
	BaseURL string
	// Bearer returns the access token for one request; nil sends none. The
	// client calls it before every request rather than taking a token once,
	// because a command can outlive the access token it started with (an
	// operation watch, a list under -w): the source hands back a refreshed
	// token after the old one expires. An error from it fails the request
	// and is returned as is.
	Bearer func(context.Context) (string, error)
	// Refresh, when set, is asked for a replacement after the API refuses
	// the bearer with 401, and the request is resent once with the token it
	// returns. It runs mid-command, so it must not start an interactive
	// login. It must leave the replacement where Bearer finds it, so later
	// requests carry it too.
	Refresh    func(ctx context.Context, rejected string) (string, error)
	HTTPClient *http.Client // nil uses a default with 30s timeout
}

// APIError carries the HTTP status and message from a failed request.
type APIError struct {
	StatusCode int
	Message    string
	// refreshErr is why a 401 could not be answered with a refreshed
	// token, when the refresher failed; see Unwrap.
	refreshErr error
}

// Unwrap returns, for a 401 the client could not answer with a refreshed
// token because the refresher failed, the refresher's error. Error does not
// show it (the 401 already names the next step), but a caller that retries
// requests can match it with errors.Is to tell a sign-in that is gone, which
// no retry clears, from a server that is refusing a token it should accept,
// which may pass.
func (e *APIError) Unwrap() error { return e.refreshErr }

// Error renders message-first: the server's explanation is what the
// operator acts on; the status code is diagnostic trivia that used to
// shout from the front of every error line. A 401 adds what to do.
func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s (HTTP %d)", e.Message, e.StatusCode)
	if e.Message == "" {
		msg = fmt.Sprintf("the API server returned HTTP %d with no detail", e.StatusCode)
	}
	if e.StatusCode == http.StatusUnauthorized {
		msg += "\n  " + unauthorizedHint
	}
	return msg
}

// unauthorizedHint is the next step after the API refused the operator's
// token (and a refresh, where the client could make one, did not help). The
// server answers every verification failure alike (an expired token, a wrong
// issuer or audience, signing keys it could not fetch), so a new sign-in is
// not always the cure; the server's log names the reason.
const unauthorizedHint = "run `slagctl auth login` to sign in again; " +
	"if the API refuses a fresh sign-in too, it is rejecting valid tokens and its logs say why"

// New creates a new Client.
func New(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	return &Client{baseURL: baseURL, bearer: cfg.Bearer, refresh: cfg.Refresh, httpClient: hc}
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
	return c.do(req, nil)
}

// BearerToken returns the access token the client would send on a request
// made now: Config.Bearer's answer, or "" when the client has none. Callers
// that open their own connections to the API (the WebSocket streams) use it
// so they authenticate exactly as the client's requests do.
func (c *Client) BearerToken(ctx context.Context) (string, error) {
	if c.bearer == nil {
		return "", nil
	}
	return c.bearer(ctx)
}

// do sends req with the bearer source's token, asked afresh for this
// request, and answers a 401 with one refresh and resend (refreshFor).
func (c *Client) do(req *http.Request, out any) error {
	token, err := c.BearerToken(req.Context())
	if err != nil {
		return err
	}
	status, b, err := c.send(req, token)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		if retry, fresh := c.refreshFor(req, token); retry != nil {
			if status, b, err = c.send(retry, fresh); err != nil {
				return err
			}
			if status == http.StatusUnauthorized {
				c.stopRefreshing()
			}
		}
	}

	if status >= 400 {
		msg := strings.TrimSpace(string(b))
		// Try to extract "message" field from JSON error body
		var errBody struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &errBody) == nil && errBody.Message != "" {
			msg = errBody.Message
		}
		apiErr := &APIError{StatusCode: status, Message: msg}
		if status == http.StatusUnauthorized {
			c.mu.Lock()
			apiErr.refreshErr = c.refreshErr
			c.mu.Unlock()
		}
		return apiErr
	}

	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// send makes one attempt at req with token as its bearer and returns the
// response status and body.
func (c *Client) send(req *http.Request, token string) (int, []byte, error) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Del("Authorization")
	}
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, c.humanizeTransportErr(req, err, time.Since(start))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, b, nil
}

// refreshFor answers a 401 to req, which carried the bearer sent: it asks the
// refresher for a replacement and returns a copy of req to resend once with
// it. Later requests get the replacement from the bearer source, which reads
// the cache the refresher wrote. It returns a nil request when no
// refresh is possible, and the caller reports the 401. Resending is safe for
// every method: the API refuses a bearer before any handler runs.
//
// A failed refresh is not shown on its own. The operator's next step is
// the same either way (sign in again, which reports any provider trouble
// itself), and the 401 says that. The failure is kept, and this 401 and
// every later one wrap it, for callers that retry (APIError.Unwrap).
func (c *Client) refreshFor(req *http.Request, sent string) (*http.Request, string) {
	c.mu.Lock()
	off := c.refresh == nil || c.refreshOff
	c.mu.Unlock()
	// Every request this package builds has either no body or one that
	// http.NewRequestWithContext can replay (GetBody); anything else cannot
	// be resent.
	if off || (req.Body != nil && req.GetBody == nil) {
		return nil, ""
	}
	fresh, err := c.refresh(req.Context(), sent)
	if err != nil {
		c.mu.Lock()
		c.refreshOff, c.refreshErr = true, err
		c.mu.Unlock()
		return nil, ""
	}
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		if retry.Body, err = req.GetBody(); err != nil {
			return nil, ""
		}
	}
	return retry, fresh
}

func (c *Client) stopRefreshing() {
	c.mu.Lock()
	c.refreshOff = true
	c.mu.Unlock()
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
