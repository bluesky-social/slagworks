// Package clicheck runs the checks behind `slagctl doctor`. Each check is a
// named function that returns a status and a detail string; the runner
// renders them as an aligned pass/warn/fail table, reports failure iff any
// check fails, and can emit JSON instead for scripts and agents.
package clicheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Status is the outcome of a single check.
type Status int

const (
	// StatusOK means the check passed.
	StatusOK Status = iota
	// StatusWarn means the check found something worth flagging but not fatal.
	StatusWarn
	// StatusFail means the check found a problem that must be resolved.
	StatusFail
)

// DefaultCheckTimeout is applied to each check that does not supply its own
// context deadline. 5 s is ample for a local stat + one cheap remote call;
// a hung network check should not wedge the entire doctor run.
const DefaultCheckTimeout = 5 * time.Second

// Result is what a Check.Run function returns.
type Result struct {
	Status Status
	Detail string
}

// OK returns a Result with StatusOK and the given detail.
func OK(detail string) Result { return Result{Status: StatusOK, Detail: detail} }

// Warn returns a Result with StatusWarn and the given detail.
func Warn(detail string) Result { return Result{Status: StatusWarn, Detail: detail} }

// Fail returns a Result with StatusFail and the given detail.
func Fail(detail string) Result { return Result{Status: StatusFail, Detail: detail} }

// Check is a named diagnostic. Run is called with a fresh context that has
// a deadline set to DefaultCheckTimeout; callers may further constrain it.
type Check struct {
	Name string
	Run  func(ctx context.Context) Result
}

// finding combines a Check with its result; used internally and for JSON output.
type finding struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func statusGlyph(s Status) string {
	switch s {
	case StatusOK:
		return "✔"
	case StatusWarn:
		return "⚠"
	default:
		return "✘"
	}
}

func statusString(s Status) string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	default:
		return "fail"
	}
}

// RunChecks executes each check in order. Each check gets a context with
// DefaultCheckTimeout; a check that does not return in time is recorded as
// Fail with "timed out". Results are rendered to w as a table with one
// status mark per line (plain text, no color, so it reads the same in a
// terminal and in captured agent output).
//
// If asJSON is true the output is a JSON array instead.
//
// Returns true iff no check has StatusFail.
func RunChecks(ctx context.Context, w io.Writer, checks []Check, asJSON bool) bool {
	findings := make([]finding, len(checks))
	for i, ch := range checks {
		r := runOne(ctx, ch)
		findings[i] = finding{
			Name:   ch.Name,
			Status: statusString(r.Status),
			Detail: r.Detail,
		}
	}

	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(findings)
		return allOK(findings)
	}

	renderTable(w, findings)
	return allOK(findings)
}

// runOne executes a single check with a timeout-guarded context.
func runOne(parent context.Context, ch Check) Result {
	ctx, cancel := context.WithTimeout(parent, DefaultCheckTimeout)
	defer cancel()

	type outcome struct{ r Result }
	done := make(chan outcome, 1)
	go func() {
		done <- outcome{ch.Run(ctx)}
	}()

	select {
	case o := <-done:
		return o.r
	case <-ctx.Done():
		return Fail("timed out")
	}
}

// renderTable prints a left-aligned name column and a right detail column
// separated by spaces. Name column width is the maximum name length across
// all findings.
func renderTable(w io.Writer, findings []finding) {
	maxLen := 0
	for _, f := range findings {
		if len(f.Name) > maxLen {
			maxLen = len(f.Name)
		}
	}
	for _, f := range findings {
		var s Status
		switch f.Status {
		case "ok":
			s = StatusOK
		case "warn":
			s = StatusWarn
		default:
			s = StatusFail
		}
		pad := strings.Repeat(" ", maxLen-len(f.Name))
		if f.Detail != "" {
			_, _ = fmt.Fprintf(w, "%s %s%s  %s\n", statusGlyph(s), f.Name, pad, f.Detail)
		} else {
			_, _ = fmt.Fprintf(w, "%s %s\n", statusGlyph(s), f.Name)
		}
	}
}

// allOK returns true iff no finding has status "fail".
func allOK(findings []finding) bool {
	for _, f := range findings {
		if f.Status == "fail" {
			return false
		}
	}
	return true
}
