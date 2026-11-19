package bmcreconcile

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// redfishRequestTimeout bounds one Redfish request. BMC web servers are
// slow (an AMI PATCH can take several seconds) but never legitimately
// take this long.
const redfishRequestTimeout = 30 * time.Second

// redfishErrorBodyLimit caps how much of an error response body is quoted
// in an error: enough for a Redfish @Message.ExtendedInfo message.
const redfishErrorBodyLimit = 512

// redfishClient is the minimal Redfish client the time pass needs: read a
// resource with its ETag, and PATCH one with If-Match.
//
// Hand-rolled over net/http rather than gofish (vendored only as an
// indirect bmclib dependency) because the writes hinge on exact If-Match
// handling, a header-level detail a typed client hides: Supermicro refuses
// `If-Match: *` with 412 and wants the resource's real ETag. Four request
// shapes do not justify a new direct dependency.
//
// It authenticates every request with HTTP basic auth rather than a
// Redfish session: AMI BMCs have few session slots, and a session the
// worker failed to delete would hold one.
type redfishClient struct {
	base       string // "https://host[:port]"
	user, pass string
	http       *http.Client
}

func newRedfishClient(addr, user, pass string, hc *http.Client) *redfishClient {
	host := addr
	if _, _, err := net.SplitHostPort(addr); err != nil && strings.Contains(addr, ":") {
		host = "[" + strings.Trim(addr, "[]") + "]" // bare IPv6 literal
	}
	return &redfishClient{base: "https://" + host, user: user, pass: pass, http: hc}
}

// defaultRedfishHTTPClient trusts any certificate: BMCs serve self-signed
// certificates on the OOB network, the same posture as bmclib's Redfish
// provider and the PDU Redfish reads (internal/power/pdu_transport.go).
func defaultRedfishHTTPClient() *http.Client {
	return &http.Client{
		Timeout: redfishRequestTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed BMC certs on the trusted OOB network, see the comment above
		},
	}
}

// checkRedfishPath refuses anything but an absolute /redfish/v1/ path.
// Resource links come from the BMC, and an absolute URL in one would send
// the admin credential to whatever host it names.
func checkRedfishPath(p string) error {
	if !strings.HasPrefix(p, "/redfish/v1/") {
		return fmt.Errorf("refusing Redfish link %q: not a /redfish/v1/ path on this BMC", p)
	}
	return nil
}

func (c *redfishClient) do(ctx context.Context, method, path string, body []byte, ifMatch string) (int, http.Header, []byte, error) {
	if err := checkRedfishPath(path); err != nil {
		return 0, nil, nil, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("redfish %s %s: %w", method, path, err)
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("redfish %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, fmt.Errorf("redfish %s %s: read body: %w", method, path, err)
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// redfishStatusError is a non-success response. Its text quotes the start
// of the body (the BMC's @Message.ExtendedInfo explains most refusals).
type redfishStatusError struct {
	method, path string
	status       int
	body         string
}

func (e *redfishStatusError) Error() string {
	return fmt.Sprintf("redfish %s %s: HTTP %d: %s", e.method, e.path, e.status, e.body)
}

func statusError(method, path string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > redfishErrorBodyLimit {
		msg = msg[:redfishErrorBodyLimit] + "..."
	}
	return &redfishStatusError{method: method, path: path, status: status, body: msg}
}

// isNotFound reports whether err is a Redfish 404.
func isNotFound(err error) bool {
	var se *redfishStatusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

// unansweredError is a PATCH that got no readable response: it timed out,
// the connection dropped, ctx ended, or the body could not be read. The
// request may have reached the BMC, so it may have been applied.
type unansweredError struct{ err error }

func (e *unansweredError) Error() string { return e.err.Error() }
func (e *unansweredError) Unwrap() error { return e.err }

// mayHaveApplied reports whether err is from a PATCH the BMC may have
// applied anyway (unansweredError). Any other patch error is a write the
// BMC never received (the ETag read failed) or refused with a status.
func mayHaveApplied(err error) bool {
	var ue *unansweredError
	return errors.As(err, &ue)
}

// get reads path into out and returns the resource's ETag: the ETag
// header, else the body's @odata.etag, else "".
func (c *redfishClient) get(ctx context.Context, path string, out any) (string, error) {
	status, hdr, raw, err := c.do(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", statusError(http.MethodGet, path, status, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return "", fmt.Errorf("redfish GET %s: decode: %w", path, err)
	}
	if etag := hdr.Get("ETag"); etag != "" {
		return etag, nil
	}
	var meta struct {
		ETag string `json:"@odata.etag"`
	}
	_ = json.Unmarshal(raw, &meta)
	return meta.ETag, nil
}

// patch writes body to path with If-Match set to the resource's ETag, read
// by a GET just before; "*" only when the BMC exposes no ETag. One 412 is
// retried with a fresh ETag: a resource that embeds a running clock (the
// Manager's DateTime) can change ETag between the GET and the PATCH.
//
// A 2xx means accepted, not applied. task is the path of the Redfish Task
// the BMC answered with, "" when the response body is not one: Supermicro
// answers an NTP PATCH 202 with a Task that applies the change and can
// fail. That is the Task resource (/redfish/v1/TaskService/Tasks/N), which
// keeps its final state, not the Location TaskMonitor, which stops
// answering with the Task once it ends.
//
// An error does not always mean not applied either: a PATCH with no
// readable response is an unansweredError (see mayHaveApplied).
func (c *redfishClient) patch(ctx context.Context, path string, body any) (task string, err error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("redfish PATCH %s: encode: %w", path, err)
	}
	for attempt := 0; ; attempt++ {
		var discard json.RawMessage
		etag, err := c.get(ctx, path, &discard)
		if err != nil {
			return "", fmt.Errorf("read ETag for PATCH: %w", err)
		}
		if etag == "" {
			etag = "*"
		}
		status, _, resp, err := c.do(ctx, http.MethodPatch, path, raw, etag)
		if err != nil {
			return "", &unansweredError{err: err}
		}
		switch {
		case status == http.StatusOK || status == http.StatusAccepted || status == http.StatusNoContent:
			return taskPath(resp), nil
		case status == http.StatusPreconditionFailed && attempt == 0:
			continue
		default:
			return "", statusError(http.MethodPatch, path, status, resp)
		}
	}
}

// taskPath returns the @odata.id of a Redfish Task response body, or ""
// when the body is not a Task (an @odata.type of #Task.*). A Task link
// that is not a path on this BMC (checkRedfishPath) counts as no Task.
func taskPath(body []byte) string {
	var t struct {
		Type string `json:"@odata.type"`
		ID   string `json:"@odata.id"`
	}
	if json.Unmarshal(body, &t) != nil || !strings.HasPrefix(t.Type, "#Task.") || checkRedfishPath(t.ID) != nil {
		return ""
	}
	return t.ID
}
