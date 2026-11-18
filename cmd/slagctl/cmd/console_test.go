package cmd

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestBuildConsoleStreamURL_Params asserts the query params each mode
// builds. The live (no-backfill) path must carry no since selector, the
// from-start path must carry since_us=0, and a --tail value must be
// emitted as a server-parseable Go duration. `--tail 80`
// (bare number) used to be passed through verbatim and the server
// 400'd the WS handshake on `time.ParseDuration("80")`.
func TestBuildConsoleStreamURL_Params(t *testing.T) {
	const (
		base = "https://slagworks.example/"
		mid  = "fd156392-803e-4f7b-b674-35b036046fc5"
	)
	cases := []struct {
		name      string
		tail      string
		fromStart bool
		follow    bool
		snapshot  bool
		wantErr   bool
		// expected query params (nil value means "must be absent")
		want map[string]*string
	}{
		{
			name:   "live_no_backfill",
			follow: false,
			want: map[string]*string{
				"machine":  new(string(mid)),
				"since_us": nil,
				"tail":     nil,
				"follow":   nil,
			},
		},
		{
			name:   "live_follow",
			follow: true,
			want: map[string]*string{
				"follow":   new("true"),
				"since_us": nil,
				"tail":     nil,
			},
		},
		{
			name:      "from_start",
			fromStart: true,
			want: map[string]*string{
				"since_us": new("0"),
				"tail":     nil,
			},
		},
		{
			name: "tail_bare_number_becomes_seconds",
			tail: "80",
			want: map[string]*string{
				"tail":     new("80s"),
				"since_us": nil,
			},
		},
		{
			name: "tail_bare_float_becomes_seconds",
			tail: "1.5",
			want: map[string]*string{
				"tail": new("1.5s"),
			},
		},
		{
			name: "tail_with_unit_passthrough",
			tail: "10m",
			want: map[string]*string{
				"tail": new("10m"),
			},
		},
		{
			name: "tail_with_seconds_unit_passthrough",
			tail: "90s",
			want: map[string]*string{
				"tail": new("90s"),
			},
		},
		{
			name:    "tail_garbage_errors_client_side",
			tail:    "soon",
			wantErr: true,
		},
		{
			// from-start wins over tail (the switch checks fromStart
			// first); a bare-number tail in that combo must not error.
			name:      "from_start_beats_tail",
			tail:      "80",
			fromStart: true,
			want: map[string]*string{
				"since_us": new("0"),
				"tail":     nil,
			},
		},
		{
			// snapshot mode: the watch TUI's mode. Carries
			// snapshot=true + follow, never a since selector.
			name:     "snapshot_follow",
			snapshot: true,
			follow:   true,
			want: map[string]*string{
				"snapshot": new("true"),
				"follow":   new("true"),
				"since_us": nil,
				"tail":     nil,
			},
		},
		{
			// snapshot wins over fromStart/tail in the switch (checked first):
			// snapshot always reflects full history, no replay floor.
			name:      "snapshot_beats_from_start",
			snapshot:  true,
			fromStart: true,
			want: map[string]*string{
				"snapshot": new("true"),
				"since_us": nil,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildConsoleStreamURL(base, mid, c.tail, c.fromStart, c.follow, c.snapshot)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got URL %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			u, perr := url.Parse(got)
			if perr != nil {
				t.Fatalf("result not a URL: %v", perr)
			}
			if u.Scheme != "wss" {
				t.Errorf("scheme = %q, want wss", u.Scheme)
			}
			if u.Path != "/api/v1/console/stream" {
				t.Errorf("path = %q, want /api/v1/console/stream", u.Path)
			}
			q := u.Query()
			for k, want := range c.want {
				_, present := q[k]
				if want == nil {
					if present {
						t.Errorf("param %q present (=%q), want absent", k, q.Get(k))
					}
					continue
				}
				if !present {
					t.Errorf("param %q absent, want %q", k, *want)
					continue
				}
				if got := q.Get(k); got != *want {
					t.Errorf("param %q = %q, want %q", k, got, *want)
				}
			}
		})
	}
}

// TestNormalizeTailDuration covers the unit-coercion contract directly.
func TestNormalizeTailDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"80", "80s", false},
		{"0", "0s", false},
		{"1.5", "1.5s", false},
		{" 30 ", "30s", false}, // trimmed then coerced
		{"10m", "10m", false},
		{"90s", "90s", false},
		{"2h30m", "2h30m", false},
		{"soon", "", true},
		{"80x", "", true},
		{"", "", true},
		// ParseFloat accepts these; ParseDuration rejects them. The
		// error must name the original input, not the "s"-appended form.
		{"NaN", "", true},
		{"Inf", "", true},
		{"1e3", "", true},
	}
	for _, c := range cases {
		got, err := normalizeTailDuration(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalizeTailDuration(%q) = %q, want error", c.in, got)
			} else if !strings.Contains(err.Error(), strconv.Quote(c.in)) {
				// The error must quote the operator's original input,
				// not the internally mutated value.
				t.Errorf("normalizeTailDuration(%q) error = %q, want it to quote %q", c.in, err.Error(), c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeTailDuration(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeTailDuration(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

//go:fix inline
