package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// clockStepThreshold is the local-vs-server skew above which syncClock
// steps the clock. The HTTP Date header has 1s resolution and the
// request adds an RTT, so anything under a few seconds is noise; 30s is
// well clear of that and well under what breaks apt Release validity or
// Tailscale's control-plane auth.
const clockStepThreshold = 30 * time.Second

// clockSourcePath is HEADed on the site worker for its Date header. It
// is the path the initrd just downloaded this binary from, so it is
// known to be routed and reachable; a HEAD on http.ServeFile sends no
// body.
const clockSourcePath = "/files/bin/slagworks-agent"

// clockFloor rejects a server Date older than this as a broken source:
// stepping a sane local clock back to a server with its own dead RTC
// would be worse than doing nothing.
var clockFloor = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// decideClockStep reports whether the clock should be stepped given the
// local time when the response arrived and the server's Date header
// value, and the server time to step to. An error means the header is
// unusable (missing, unparseable, or before clockFloor); callers must
// not step in that case.
func decideClockStep(local time.Time, dateHeader string) (time.Time, bool, error) {
	if dateHeader == "" {
		return time.Time{}, false, errors.New("no Date header in site worker response")
	}
	server, err := http.ParseTime(dateHeader)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("unparseable Date header %q: %w", dateHeader, err)
	}
	if server.Before(clockFloor) {
		return time.Time{}, false, fmt.Errorf("site worker Date %s is before %s; its clock is wrong, not trusting it",
			server.Format(time.RFC3339), clockFloor.Format(time.RFC3339))
	}
	skew := local.Sub(server)
	if skew < 0 {
		skew = -skew
	}
	return server, skew > clockStepThreshold, nil
}

// clockSetter is the syscall seam: the production implementation
// (sysClock) needs root and real hardware; tests inject a fake.
type clockSetter interface {
	SetSystemTime(time.Time) error
	SetRTC(time.Time) error
}

// syncClock corrects the system clock from the site worker's HTTP Date
// header and persists it to the RTC so the deployed OS boots with sane
// time. A host whose CMOS battery died boots the initrd at the RTC's
// reset epoch (pop1 leaf-3: 2009-01-01); left uncorrected, Tailscale
// enrollment fails and the deployed OS's first-boot apt rejects Release
// files as "not valid yet". Every failure here is logged and swallowed:
// clock sync must never fail a deploy.
func syncClock(log *slog.Logger, server string, client *http.Client, clk clockSetter) {
	url := "http://" + server + clockSourcePath
	req, err := http.NewRequest(http.MethodHead, url, nil) //nolint:noctx // client.Timeout bounds the request
	if err != nil {
		log.Warn("clock sync: building request failed, leaving clock as-is", "url", url, "err", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Warn("clock sync: site worker unreachable, leaving clock as-is", "url", url, "err", err)
		return
	}
	recv := time.Now()
	_ = resp.Body.Close()

	serverTime, step, err := decideClockStep(recv, resp.Header.Get("Date"))
	if err != nil {
		log.Warn("clock sync: leaving clock as-is", "err", err)
		return
	}
	if !step {
		log.Info("clock sync: local clock within threshold of site worker",
			"local", recv.UTC().Format(time.RFC3339), "server", serverTime.Format(time.RFC3339))
		return
	}

	// Advance the server's timestamp by the (monotonic) time elapsed
	// since the response arrived, so the step lands on "now".
	target := serverTime.Add(time.Since(recv))
	old := time.Now()
	if err := clk.SetSystemTime(target); err != nil {
		log.Warn("clock sync: setting system clock failed, leaving clock as-is",
			"local", old.UTC().Format(time.RFC3339), "server", target.UTC().Format(time.RFC3339), "err", err)
		return
	}
	log.Info("clock sync: stepped system clock from site worker Date",
		"old", old.UTC().Format(time.RFC3339), "new", target.UTC().Format(time.RFC3339),
		"skew", recv.Sub(serverTime).Round(time.Second).String())

	if err := clk.SetRTC(target); err != nil {
		log.Warn("clock sync: writing RTC failed; the deployed OS may boot with the old hardware clock", "err", err)
		return
	}
	log.Info("clock sync: wrote RTC (UTC)", "time", target.UTC().Format(time.RFC3339))
}
