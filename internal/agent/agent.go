// Package agent holds the shared context the slagworks-agent's
// subsystem packages (execkit, storageapply, bootloader, netapply,
// target) receive instead of reaching through package globals.
// Constructed once in cmd/slagworks-agent main() and passed explicitly,
// so each subsystem is independently testable with a fake Runner and a
// test logger.
package agent

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/bluesky-social/slagworks/internal/agent/execkit"
)

// Runner abstracts command execution. The production implementation is
// execkit.Exec; tests inject fakes (see cmd/slagworks-agent's
// fixtureRunner) so subsystem logic runs without spawning processes.
type Runner interface {
	// Run executes a command, teeing its output to the agent's live
	// log surfaces, and returns the command's error. The full exec
	// outcome (argv, exit code, duration, bounded stdout/stderr)
	// ships as a structured log envelope.
	Run(name string, args ...string) error
	// Output executes a command and returns its trimmed stdout.
	Output(name string, args ...string) (string, error)
	// Available reports whether the named command exists in $PATH.
	Available(name string) bool
}

// Context carries the agent's wiring: the structured logger (the ONLY
// output surface — no fmt.Print* in subsystem code), the exec seam,
// and the site worker address every fetch goes through.
type Context struct {
	Log    *slog.Logger
	Exec   Runner
	Server string // site worker host:port
	// Procs is the group every process the agent starts outside Exec runs
	// in (config scripts, the tailscale daemon and client), the same group
	// the production Exec uses, so that stopping the agent ends all of
	// them. nil, as in tests, leaves those processes untracked.
	Procs *execkit.Group
}

// HTTPGet fetches a URL from the site worker and returns the response
// body, or nil for a 204 (the "nothing configured/matched" signal the
// site's per-machine endpoints use).
func HTTPGet(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// PostOptions tunes PostJSON's retry behavior. The zero value is a
// single-attempt fire-and-forget POST (Attempts defaults to 1). Callers
// that must not lose the report set Attempts > 1 and a backoff shape.
type PostOptions struct {
	// Client is the HTTP client to use. Required.
	Client *http.Client
	// Attempts is the total number of POST attempts (not retries). A
	// value < 1 is treated as 1 (a single attempt, no retry).
	Attempts int
	// Backoff is the sleep before the SECOND attempt. Zero means no
	// sleep between attempts (they fire back-to-back).
	Backoff time.Duration
	// BackoffMult multiplies Backoff after each attempt (exponential
	// backoff). A value <= 1 keeps Backoff flat across retries. Ignored
	// when Attempts == 1.
	BackoffMult int
	// Name labels the request in retry log lines (e.g. "deploy_complete
	// report", "inventory report").
	Name string
	// AcceptFn decides whether an HTTP status counts as success. When
	// nil, any 2xx is accepted. Callers requiring an exact status pass
	// their own predicate (e.g. func(s int) bool { return s == 200 }).
	AcceptFn func(status int) bool
	// FinalFn names statuses that are the server's final answer: PostJSON
	// returns the StatusError at once instead of spending the remaining
	// attempts on a request that cannot succeed. Checked before AcceptFn.
	// nil means no status is final.
	FinalFn func(status int) bool
}

// StatusError is the error PostJSON returns for a response whose status was
// not accepted. Callers that act on a particular status find it with
// errors.As.
type StatusError struct {
	Status int
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Status) }

// PostJSON POSTs a pre-marshaled JSON body to url, retrying per opts, and
// returns nil once a response with an accepted status arrives. It is the
// single POST-with-retry path for the agent's report/inventory/commission
// calls, replacing three divergent hand-rolled loops.
//
// Response bodies are drained-and-closed but never read — none of the
// site worker's report endpoints return a body the agent consumes; the
// status code is the whole contract.
//
// Retry attempts (both transport errors and unaccepted statuses) log a
// Warn through log so they land in the machine-logs envelope feed, not
// just the agent's local stdout. A nil log disables that logging.
func PostJSON(log *slog.Logger, url string, body []byte, opts PostOptions) error {
	if opts.Client == nil {
		// Client is documented Required; fail loudly with an error a
		// checking caller can act on rather than a nil-deref panic
		// inside net/http.
		return fmt.Errorf("%s: PostOptions.Client is required", opts.Name)
	}
	attempts := opts.Attempts
	if attempts < 1 {
		attempts = 1
	}
	accept := opts.AcceptFn
	if accept == nil {
		accept = func(status int) bool { return status >= 200 && status < 300 }
	}
	backoff := opts.Backoff
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		resp, err := opts.Client.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx
		if err != nil {
			lastErr = err
			if log != nil {
				log.Warn(opts.Name+" attempt failed", "attempt", attempt, "max", attempts, "err", err)
			}
		} else {
			status := resp.StatusCode
			resp.Body.Close()
			if opts.FinalFn != nil && opts.FinalFn(status) {
				return fmt.Errorf("%s refused: %w", opts.Name, &StatusError{Status: status})
			}
			if accept(status) {
				return nil
			}
			lastErr = &StatusError{Status: status}
			if log != nil {
				log.Warn(opts.Name+" attempt returned unaccepted status", "attempt", attempt, "max", attempts, "status", status)
			}
		}
		if attempt < attempts {
			if backoff > 0 {
				time.Sleep(backoff)
			}
			if opts.BackoffMult > 1 {
				backoff *= time.Duration(opts.BackoffMult)
			}
		}
	}
	if attempts == 1 {
		// Fire-and-forget callers ignore the return; still surface the
		// single-attempt error for callers that check.
		return lastErr
	}
	return fmt.Errorf("%s failed after %d attempts: %w", opts.Name, attempts, lastErr)
}
