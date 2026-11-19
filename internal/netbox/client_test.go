package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(ClientConfig{
		BaseURL:    srv.URL,
		Token:      "test-token",
		APIVersion: "4.3",
		Timeout:    5 * time.Second,
	})
}

func TestAuth(t *testing.T) {
	var gotAuth, gotAccept string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))

	var dest map[string]string
	if err := c.Get(context.Background(), "/api/test/", nil, &dest); err != nil {
		t.Fatal(err)
	}

	if gotAuth != "Token test-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Token test-token")
	}
	if gotAccept != "application/json; version=4.3" {
		t.Errorf("Accept = %q, want %q", gotAccept, "application/json; version=4.3")
	}
}

func TestPost(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 42, "name": body["name"]})
	}))

	var dest struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	err := c.Post(context.Background(), "/api/dcim/devices/", map[string]string{"name": "srv01"}, &dest)
	if err != nil {
		t.Fatal(err)
	}
	if dest.ID != 42 || dest.Name != "srv01" {
		t.Errorf("got %+v, want id=42 name=srv01", dest)
	}
}

func TestDelete(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	if err := c.Delete(context.Background(), "/api/dcim/devices/1/"); err != nil {
		t.Fatal(err)
	}
}

func TestAPIError(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"detail":"Not found."}`))
	}))

	var dest map[string]string
	err := c.Get(context.Background(), "/api/dcim/devices/999/", nil, &dest)
	if err == nil {
		t.Fatal("expected error")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 404 {
		t.Errorf("status = %d, want 404", apiErr.StatusCode)
	}
}

func TestRateLimitRetry(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	c.retryBaseWait = 10 * time.Millisecond // speed up test

	var dest map[string]string
	if err := c.Get(context.Background(), "/api/test/", nil, &dest); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestRateLimitExhausted(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	c.retryBaseWait = 10 * time.Millisecond
	c.maxRetries = 2

	err := c.Get(context.Background(), "/api/test/", nil, nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.StatusCode != 429 {
		t.Errorf("expected 429 APIError, got %v", err)
	}
}

// TestRateLimitDefaultBudgetAbsorbsThree429s pins the documented default:
// MaxRetries 3 means three retries after the first attempt, so three 429s
// followed by a 200 must succeed on the fourth request. do() used to count
// the first attempt against the budget and gave up after the third request.
func TestRateLimitDefaultBudgetAbsorbsThree429s(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	c.retryBaseWait = time.Millisecond

	var dest map[string]string
	if err := c.Get(context.Background(), "/api/test/", nil, &dest); err != nil {
		t.Fatalf("three 429s with the default MaxRetries (3) should be retried, got: %v (calls=%d)", err, calls.Load())
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("calls = %d, want 4 (1 attempt + 3 retries)", got)
	}
}

// TestRateLimitRetriesMaxRetriesTimes pins the request count against a
// server that never stops answering 429: one attempt plus MaxRetries
// retries, for several budgets. MaxRetries 1 used to perform no retry.
func TestRateLimitRetriesMaxRetriesTimes(t *testing.T) {
	for _, maxRetries := range []int{1, 2, 3, 5} {
		t.Run(fmt.Sprintf("MaxRetries=%d", maxRetries), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(srv.Close)
			c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "test-token", MaxRetries: maxRetries})
			c.retryBaseWait = time.Millisecond

			err := c.Get(context.Background(), "/api/test/", nil, nil)
			apiErr, ok := err.(*APIError)
			if !ok || apiErr.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("expected 429 APIError from an always-429 server, got %v", err)
			}
			if got, want := int(calls.Load()), maxRetries+1; got != want {
				t.Errorf("calls = %d, want %d (1 attempt + %d retries)", got, want, maxRetries)
			}
		})
	}
}

func TestPagination(t *testing.T) {
	// Simulate 3 pages of results.
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		switch offset {
		case "", "0":
			next := fmt.Sprintf("http://%s/api/test/?limit=2&offset=2", r.Host)
			json.NewEncoder(w).Encode(ListResponse[map[string]int]{
				Count:   5,
				Next:    &next,
				Results: []map[string]int{{"id": 1}, {"id": 2}},
			})
		case "2":
			next := fmt.Sprintf("http://%s/api/test/?limit=2&offset=4", r.Host)
			json.NewEncoder(w).Encode(ListResponse[map[string]int]{
				Count:   5,
				Next:    &next,
				Results: []map[string]int{{"id": 3}, {"id": 4}},
			})
		case "4":
			json.NewEncoder(w).Encode(ListResponse[map[string]int]{
				Count:   5,
				Results: []map[string]int{{"id": 5}},
			})
		}
	}))

	var results []struct {
		ID int `json:"id"`
	}
	if err := c.List(context.Background(), "/api/test/", nil, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 5 {
		t.Fatalf("got %d results, want 5", len(results))
	}
	for i, r := range results {
		if r.ID != i+1 {
			t.Errorf("results[%d].ID = %d, want %d", i, r.ID, i+1)
		}
	}
}

// TestPaginationUnderBasePath covers a NETBOX_URL with a path component
// ("https://host/netbox"). NetBox builds its next link from the absolute
// request URI, so where NetBox itself runs under BASE_PATH the link carries
// the prefix; where a reverse proxy strips the prefix in front of a
// root-mounted NetBox it does not. Every page must be requested under the
// configured base exactly once in both shapes. The BASE_PATH case used to
// request page 2 at /netbox/netbox/api/... and fail the whole List with a
// 404.
func TestPaginationUnderBasePath(t *testing.T) {
	for _, tc := range []struct {
		name       string
		nextPrefix string // path prefix NetBox puts on its next links
	}{
		{name: "netbox BASE_PATH", nextPrefix: "/netbox"},
		{name: "prefix-stripping proxy", nextPrefix: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				http.NotFound(w, r)
			})
			mux.HandleFunc("/netbox/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.URL.Query().Get("site") != "s1" {
					t.Errorf("request %s lost the site filter", r.URL)
				}
				if r.URL.Query().Get("offset") == "" {
					next := "http://" + r.Host + tc.nextPrefix + "/api/dcim/devices/?limit=1&offset=1&site=s1"
					_, _ = w.Write([]byte(`{"count":2,"next":"` + next + `","results":[{"id":1}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"count":2,"next":null,"results":[{"id":2}]}`))
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			c := NewClient(ClientConfig{BaseURL: srv.URL + "/netbox/", Token: "t"})

			var got []Device
			q := url.Values{"limit": {"1"}, "site": {"s1"}}
			if err := c.List(context.Background(), "/api/dcim/devices/", q, &got); err != nil {
				t.Fatalf("List under base path /netbox: %v (requested paths %v)", err, paths)
			}
			if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
				t.Fatalf("got %+v, want devices 1 and 2", got)
			}
			want := []string{"/netbox/api/dcim/devices/", "/netbox/api/dcim/devices/"}
			if fmt.Sprint(paths) != fmt.Sprint(want) {
				t.Errorf("requested paths %v, want %v", paths, want)
			}
		})
	}
}

// TestLargeBodyNotTruncated is the regression guard for the pop1
// snapshot freeze: a list response larger than 1MB must decode fully,
// not get silently truncated mid-JSON (which surfaced as the misleading
// "decoding response: unexpected end of JSON input"). The old code read
// through io.LimitReader(resp.Body, 1<<20); once pop1's interface-list
// response crossed 1MB the decode failed on every snapshot rebuild.
func TestLargeBodyNotTruncated(t *testing.T) {
	// Build a single-page list response well over 1MB. Each element is a
	// padded object; ~1500 of them clears the old 1MB cap comfortably.
	type item struct {
		ID  int    `json:"id"`
		Pad string `json:"pad"`
	}
	items := make([]item, 1500)
	for i := range items {
		items[i] = item{ID: i + 1, Pad: strings.Repeat("x", 800)}
	}
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ListResponse[item]{Count: len(items), Results: items})
	}))

	var results []item
	if err := c.List(context.Background(), "/api/test/", nil, &results); err != nil {
		t.Fatalf("List on a >1MB body failed (truncation regression?): %v", err)
	}
	if len(results) != len(items) {
		t.Fatalf("got %d results, want %d (body was truncated)", len(results), len(items))
	}
	if results[len(results)-1].ID != len(items) {
		t.Errorf("last result ID = %d, want %d", results[len(results)-1].ID, len(items))
	}
}

// TestOversizeBodyFailsLoudly verifies the cap fails with an explicit
// error rather than silently truncating, when a response actually
// exceeds the sanity ceiling.
func TestOversizeBodyFailsLoudly(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	c.maxRespBytes = 8 // absurdly low (int64) so a trivial body trips it

	var dest map[string]string
	err := c.Get(context.Background(), "/api/test/", nil, &dest)
	if err == nil {
		t.Fatal("expected an explicit over-cap error, got nil (silent truncation?)")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to name the cap overflow", err)
	}
}

func TestQueryParams(t *testing.T) {
	var gotQuery url.Values
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		json.NewEncoder(w).Encode(map[string]string{})
	}))

	q := url.Values{"role": {"server"}, "site": {"pop-a"}}
	c.Get(context.Background(), "/api/dcim/devices/", q, nil)

	if gotQuery.Get("role") != "server" || gotQuery.Get("site") != "pop-a" {
		t.Errorf("query = %v, want role=server&site=pop-a", gotQuery)
	}
}

func TestStatus(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"netbox-version": "4.3.2"})
	}))

	v, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "4.3.2" {
		t.Errorf("version = %q, want 4.3.2", v)
	}
}

func TestCheckVersion(t *testing.T) {
	tests := []struct {
		version          string
		minMajor, minMin int
		wantErr          bool
	}{
		{"4.3.2", 4, 3, false},
		{"4.4.0", 4, 3, false},
		{"5.0.0", 4, 3, false},
		{"4.2.9", 4, 3, true},
		{"3.7.0", 4, 3, true},
		{"4.3.0-beta1", 4, 3, false},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"netbox-version": tt.version})
			}))

			err := c.CheckVersion(context.Background(), tt.minMajor, tt.minMin)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckVersion(%s, %d.%d) error = %v, wantErr %v",
					tt.version, tt.minMajor, tt.minMin, err, tt.wantErr)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input        string
		major, minor int
		wantErr      bool
	}{
		{"4.3.2", 4, 3, false},
		{"4.3", 4, 3, false},
		{"4.3-beta1", 4, 3, false},
		{"4", 0, 0, true},
		{"abc.def", 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			major, minor, err := parseVersion(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseVersion(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if err == nil && (major != tt.major || minor != tt.minor) {
				t.Errorf("parseVersion(%q) = %d.%d, want %d.%d", tt.input, major, minor, tt.major, tt.minor)
			}
		})
	}
}
