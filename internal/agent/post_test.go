package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestPostJSON_FinalStatusEndsTheRetries pins FinalFn: a status the caller
// names as the server's final answer comes back at once as a StatusError,
// without the remaining attempts. The agent's final reports retry for half a
// minute; spending that on an operation the site worker has already declared
// over would keep an aborted run alive for no reason.
func TestPostJSON_FinalStatusEndsTheRetries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	err := PostJSON(nil, srv.URL, []byte(`{}`), PostOptions{
		Client:   srv.Client(),
		Attempts: 4,
		Backoff:  time.Minute, // a retry would hang the test, not pass it
		Name:     "test report",
		FinalFn:  func(status int) bool { return status == http.StatusGone },
	})
	se, ok := errors.AsType[*StatusError](err)
	if !ok || se.Status != http.StatusGone {
		t.Fatalf("err = %v, want a StatusError carrying 410", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("server saw %d requests, want 1: a final status must not be retried", got)
	}
}

// TestPostJSON_UnacceptedStatusIsAStatusError pins that an ordinary refused
// status still exhausts the attempts, and that the error names the status.
func TestPostJSON_UnacceptedStatusIsAStatusError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	err := PostJSON(nil, srv.URL, []byte(`{}`), PostOptions{
		Client:   srv.Client(),
		Attempts: 3,
		Name:     "test report",
		FinalFn:  func(status int) bool { return status == http.StatusGone },
	})
	se, ok := errors.AsType[*StatusError](err)
	if !ok || se.Status != http.StatusConflict {
		t.Fatalf("err = %v, want a StatusError carrying 409", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server saw %d requests, want all 3 attempts", got)
	}
}
