package netbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a thin HTTP client for the NetBox REST API.
// It handles authentication, API versioning, pagination, and rate limiting.
type Client struct {
	baseURL    string
	token      string
	apiVersion string
	httpClient *http.Client
	log        *slog.Logger

	// Rate limit retry config
	maxRetries    int
	retryBaseWait time.Duration

	// maxRespBytes is the sanity ceiling on a single response body. Reads
	// past it fail loudly rather than silently truncating (which would
	// hand a half-JSON document to the decoder). Defaults to 64MB.
	maxRespBytes int64
}

type ClientConfig struct {
	// BaseURL is the NetBox instance URL (e.g., "https://netbox.example.com"),
	// optionally with a path ("https://netbox.example.com/netbox").
	BaseURL string
	// Token is the NetBox API token.
	Token string
	// APIVersion pins the API version header (default "4.6").
	// NetBox 406s any Accept header whose version doesn't match the
	// server's own major.minor, so this MUST track NetBox upgrades
	// (2026-07-10: the 4.5→4.6 server upgrade crash-looped slagworksd
	// global for hours). Override without a release via NETBOX_API_VERSION.
	APIVersion string
	// Timeout for individual HTTP requests (default 10s).
	Timeout time.Duration
	// MaxRetries is how many times a request answered with 429 is retried
	// after the first attempt, with exponential backoff starting at 1s
	// (default 3: up to 4 requests over 1s+2s+4s of waiting).
	MaxRetries int
	// HTTPClient allows injecting a custom client for testing.
	HTTPClient *http.Client
}

func NewClient(cfg ClientConfig) *Client {
	if cfg.APIVersion == "" {
		cfg.APIVersion = "4.6"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.Timeout}
	}

	baseURL := strings.TrimRight(cfg.BaseURL, "/")

	return &Client{
		baseURL:       baseURL,
		token:         cfg.Token,
		apiVersion:    cfg.APIVersion,
		httpClient:    httpClient,
		log:           slog.Default(),
		maxRetries:    cfg.MaxRetries,
		retryBaseWait: 1 * time.Second,
		maxRespBytes:  64 << 20, // 64MB
	}
}

func (c *Client) SetLogger(l *slog.Logger) {
	c.log = l
}

// ListResponse is the paginated envelope NetBox wraps list results in.
type ListResponse[T any] struct {
	Count    int     `json:"count"`
	Next     *string `json:"next"`
	Previous *string `json:"previous"`
	Results  []T     `json:"results"`
}

// Get performs a GET request and decodes the JSON response into dest.
func (c *Client) Get(ctx context.Context, path string, query url.Values, dest any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, dest)
}

// Post performs a POST request with a JSON body and decodes the response.
func (c *Client) Post(ctx context.Context, path string, body, dest any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, dest)
}

// Patch performs a PATCH request with a JSON body and decodes the response.
func (c *Client) Patch(ctx context.Context, path string, body, dest any) error {
	return c.do(ctx, http.MethodPatch, path, nil, body, dest)
}

// Delete performs a DELETE request. NetBox returns 204 with no body on success.
func (c *Client) Delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

// List fetches all pages from a paginated endpoint and returns the collected results.
func (c *Client) List(ctx context.Context, path string, query url.Values, dest any) error {
	// dest must be a pointer to a slice; we accumulate via raw JSON.
	return c.listInto(ctx, path, query, dest)
}

func (c *Client) listInto(ctx context.Context, path string, query url.Values, dest any) error {
	if query == nil {
		query = url.Values{}
	}
	// Request a large page size to minimize round-trips.
	if query.Get("limit") == "" {
		query.Set("limit", "1000")
	}

	var allResults []json.RawMessage
	currentQuery := query

	for {
		var page ListResponse[json.RawMessage]
		if err := c.do(ctx, http.MethodGet, path, currentQuery, nil, &page); err != nil {
			return err
		}
		allResults = append(allResults, page.Results...)

		if page.Next == nil || *page.Next == "" {
			break
		}

		// NetBox builds the next link from the absolute request URI and
		// changes only its query (offset, or start in cursor mode), so the
		// next page is at the same endpoint: take only the query. The
		// link's host is NetBox's view of itself (possibly an internal name
		// behind a proxy), and its path includes the base URL's path prefix
		// when NetBox runs under BASE_PATH but not behind a prefix-stripping
		// proxy, so neither can be joined onto the configured base URL.
		nextQuery, err := nextPageQuery(*page.Next)
		if err != nil {
			return fmt.Errorf("parsing pagination next URL: %w", err)
		}
		currentQuery = nextQuery
	}

	// Marshal collected results back to JSON and decode into dest.
	collected, err := json.Marshal(allResults)
	if err != nil {
		return fmt.Errorf("marshaling collected results: %w", err)
	}
	return json.Unmarshal(collected, dest)
}

func nextPageQuery(nextURL string) (url.Values, error) {
	u, err := url.Parse(nextURL)
	if err != nil {
		return nil, err
	}
	return u.Query(), nil
}

// APIError represents a non-success HTTP response from NetBox.
type APIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("netbox: %s: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("netbox: %s", e.Status)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, dest any) error {
	var retries int
	for {
		err := c.doOnce(ctx, method, path, query, body, dest)
		if err == nil {
			return nil
		}

		apiErr, ok := err.(*APIError)
		if !ok || apiErr.StatusCode != http.StatusTooManyRequests {
			return err
		}

		// The first request is not a retry: give up only once maxRetries
		// retries have already been spent.
		if retries >= c.maxRetries {
			return err
		}
		retries++

		wait := c.retryBaseWait * time.Duration(1<<(retries-1))
		c.log.Warn("netbox rate limited, retrying", "retry", retries, "max_retries", c.maxRetries, "wait", wait)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (c *Client) doOnce(ctx context.Context, method, path string, query url.Values, body, dest any) error {
	reqURL := c.baseURL + path
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json; version="+c.apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("netbox %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// 204 No Content (DELETE success) — nothing to decode.
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}

	// Cap the body to guard against a runaway/malformed response, but
	// read one byte past the cap so we can tell "exactly at the cap" from
	// "over it" and fail LOUDLY on truncation. Silently truncating with a
	// bare io.LimitReader feeds a half-JSON document to the decoder, which
	// then fails with the misleading "unexpected end of JSON input" —
	// exactly the bug that froze the pop1 snapshot once its interface-list
	// response crossed 1MB. The cap is a sanity ceiling, not a page size:
	// NetBox list pages (limit=500) legitimately run to several MB.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, c.maxRespBytes+1))
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(respBody),
		}
	}

	// Truncation guard applies only to success responses we're about to
	// decode. An over-cap error body must still surface as an *APIError
	// above so the status code (e.g. 429) drives rate-limit retries rather
	// than being masked by a generic cap error.
	if int64(len(respBody)) > c.maxRespBytes {
		return fmt.Errorf("netbox %s %s: response exceeds %d-byte cap; refusing to decode a truncated body",
			method, path, c.maxRespBytes)
	}

	if dest != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, dest); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}

	return nil
}

// Status queries GET /api/status/ and returns the NetBox version string.
func (c *Client) Status(ctx context.Context) (string, error) {
	var status struct {
		NetBoxVersion string `json:"netbox-version"`
	}
	if err := c.Get(ctx, "/api/status/", nil, &status); err != nil {
		return "", err
	}
	return status.NetBoxVersion, nil
}

// CheckVersion verifies the NetBox instance is at least the given minimum version.
// Version strings are compared as major.minor (patch is ignored).
func (c *Client) CheckVersion(ctx context.Context, minMajor, minMinor int) error {
	version, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("checking NetBox version: %w", err)
	}

	major, minor, err := parseVersion(version)
	if err != nil {
		return fmt.Errorf("parsing NetBox version %q: %w", version, err)
	}

	if major < minMajor || (major == minMajor && minor < minMinor) {
		return fmt.Errorf("NetBox version %s is below minimum required %d.%d", version, minMajor, minMinor)
	}

	return nil
}

func parseVersion(v string) (int, int, error) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("expected major.minor, got %q", v)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	// Minor may have a suffix like "3-beta1"
	minorStr, _, _ := strings.Cut(parts[1], "-")
	minor, err := strconv.Atoi(minorStr)
	if err != nil {
		return 0, 0, err
	}
	return major, minor, nil
}
