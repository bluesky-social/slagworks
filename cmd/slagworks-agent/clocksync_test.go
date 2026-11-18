//go:build !integration

package main

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDecideClockStep(t *testing.T) {
	server := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)
	header := server.Format(http.TimeFormat)

	tests := []struct {
		name     string
		local    time.Time
		header   string
		wantStep bool
		wantErr  bool
	}{
		{"in sync", server, header, false, false},
		{"skew below threshold", server.Add(-29 * time.Second), header, false, false},
		{"skew below threshold, local ahead", server.Add(29 * time.Second), header, false, false},
		{"skew above threshold", server.Add(-31 * time.Second), header, true, false},
		{"skew above threshold, local ahead", server.Add(31 * time.Second), header, true, false},
		// The pop1 leaf-3 case: dead CMOS battery, RTC reset to 2009.
		{"rtc reset to 2009", time.Date(2009, 1, 1, 0, 5, 0, 0, time.UTC), header, true, false},
		{"missing header", server.Add(-time.Hour), "", false, true},
		{"unparseable header", server.Add(-time.Hour), "yesterday-ish", false, true},
		{"server time absurdly old", server, time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat), false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, step, err := decideClockStep(tt.local, tt.header)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if step != tt.wantStep {
				t.Fatalf("step = %v, want %v", step, tt.wantStep)
			}
			if step && !got.Equal(server) {
				t.Errorf("target = %v, want %v", got, server)
			}
		})
	}
}

type fakeClock struct {
	sysSet, rtcSet []time.Time
	sysErr, rtcErr error
}

func (f *fakeClock) SetSystemTime(t time.Time) error {
	f.sysSet = append(f.sysSet, t)
	return f.sysErr
}

func (f *fakeClock) SetRTC(t time.Time) error {
	f.rtcSet = append(f.rtcSet, t)
	return f.rtcErr
}

func dateServer(t *testing.T, date string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != clockSourcePath {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		// Go's server stamps Date itself unless the key is present;
		// a nil slice suppresses it (the missing-header case).
		if date == "" {
			w.Header()["Date"] = nil
		} else {
			w.Header().Set("Date", date)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSyncClockSteps(t *testing.T) {
	// The server is a year ahead of the test host's real clock, which
	// stands in for "local clock is badly behind".
	serverNow := time.Now().UTC().Add(365 * 24 * time.Hour).Truncate(time.Second)
	srv := dateServer(t, serverNow.Format(http.TimeFormat))
	fc := &fakeClock{}

	syncClock(discardLog(), strings.TrimPrefix(srv.URL, "http://"), srv.Client(), fc)

	if len(fc.sysSet) != 1 || len(fc.rtcSet) != 1 {
		t.Fatalf("sysSet=%v rtcSet=%v, want one of each", fc.sysSet, fc.rtcSet)
	}
	// Target is the server's Date plus the (tiny) elapsed time since
	// the response arrived.
	if d := fc.sysSet[0].Sub(serverNow); d < 0 || d > 5*time.Second {
		t.Errorf("system time set to %v, want ~%v", fc.sysSet[0], serverNow)
	}
	if !fc.rtcSet[0].Equal(fc.sysSet[0]) {
		t.Errorf("rtc set to %v, system to %v; want equal", fc.rtcSet[0], fc.sysSet[0])
	}
}

func TestSyncClockNoStepWhenInSync(t *testing.T) {
	srv := dateServer(t, time.Now().UTC().Format(http.TimeFormat))
	fc := &fakeClock{}

	syncClock(discardLog(), strings.TrimPrefix(srv.URL, "http://"), srv.Client(), fc)

	if len(fc.sysSet) != 0 || len(fc.rtcSet) != 0 {
		t.Errorf("sysSet=%v rtcSet=%v, want none", fc.sysSet, fc.rtcSet)
	}
}

func TestSyncClockNoDateHeader(t *testing.T) {
	srv := dateServer(t, "")
	fc := &fakeClock{}

	syncClock(discardLog(), strings.TrimPrefix(srv.URL, "http://"), srv.Client(), fc)

	if len(fc.sysSet) != 0 || len(fc.rtcSet) != 0 {
		t.Errorf("sysSet=%v rtcSet=%v, want none", fc.sysSet, fc.rtcSet)
	}
}

func TestSyncClockServerUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	fc := &fakeClock{}

	syncClock(discardLog(), addr, &http.Client{Timeout: time.Second}, fc)

	if len(fc.sysSet) != 0 || len(fc.rtcSet) != 0 {
		t.Errorf("sysSet=%v rtcSet=%v, want none", fc.sysSet, fc.rtcSet)
	}
}

func TestSyncClockSystemSetFailureSkipsRTC(t *testing.T) {
	serverNow := time.Now().UTC().Add(365 * 24 * time.Hour)
	srv := dateServer(t, serverNow.Format(http.TimeFormat))
	fc := &fakeClock{sysErr: errors.New("EPERM")}

	syncClock(discardLog(), strings.TrimPrefix(srv.URL, "http://"), srv.Client(), fc)

	if len(fc.sysSet) != 1 {
		t.Fatalf("sysSet=%v, want one attempt", fc.sysSet)
	}
	if len(fc.rtcSet) != 0 {
		t.Errorf("rtcSet=%v, want none after system-clock failure", fc.rtcSet)
	}
}

func TestSyncClockRTCFailureIsNonFatal(t *testing.T) {
	serverNow := time.Now().UTC().Add(365 * 24 * time.Hour)
	srv := dateServer(t, serverNow.Format(http.TimeFormat))
	fc := &fakeClock{rtcErr: errors.New("open /dev/rtc0: no such file or directory")}

	// Must return normally (no panic, no exit).
	syncClock(discardLog(), strings.TrimPrefix(srv.URL, "http://"), srv.Client(), fc)

	if len(fc.sysSet) != 1 || len(fc.rtcSet) != 1 {
		t.Errorf("sysSet=%v rtcSet=%v, want one of each", fc.sysSet, fc.rtcSet)
	}
}
