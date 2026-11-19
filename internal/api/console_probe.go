package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/store"
)

// consoleProbe answers "did probe nonce N arrive in machine M's
// console_cast_events stream within the last L seconds?"
//
// The agent emits the marker `SLAG-CONSOLE-PROBE-<machine>-<nonce>`
// during commission; this endpoint lets the operator (or
// an automated test) confirm the bytes round-tripped through BMC SOL
// → site worker pump → global's cast events.
//
// Query params:
//
//	nonce=<hex>      required; 8-32 chars, the value the agent emitted
//	lookback=<sec>   optional; default 300 (5 min), max 3600.
//
// Responses:
//
//	200 {"observed": true}  — nonce found
//	200 {"observed": false} — no match in the window (or no cast yet)
//	400                     — bad params
func (s *Server) consoleProbe(c *echo.Context) error {
	machineID, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	nonce := c.QueryParam("nonce")
	if nonce == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "nonce required")
	}
	if len(nonce) > 64 {
		return echo.NewHTTPError(http.StatusBadRequest, "nonce too long")
	}
	// Reject anything outside hex. The agent emits 16 hex chars
	// from rand.Read, so any other value cannot name one of its
	// markers.
	for _, ch := range nonce {
		isHex := (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
		if !isHex {
			return echo.NewHTTPError(http.StatusBadRequest, "nonce must be hex")
		}
	}
	lookback := 5 * time.Minute
	if v := c.QueryParam("lookback"); v != "" {
		secs, perr := strconv.Atoi(v)
		if perr != nil || secs < 1 || secs > 3600 {
			return echo.NewHTTPError(http.StatusBadRequest, "lookback must be 1..3600")
		}
		lookback = time.Duration(secs) * time.Second
	}

	hit, err := s.consoleStore.ScanCastEventsForNonce(c.Request().Context(), machineID, nonce, lookback)
	if err != nil {
		// Cast row not yet created (no SOL bytes ever observed for
		// this machine) is the same answer as "no match" from the
		// caller's perspective.
		if errors.Is(err, store.ErrConsoleCastNotFound) {
			return c.JSON(http.StatusOK, map[string]bool{"observed": false})
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]bool{"observed": hit})
}
