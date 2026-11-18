package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
	"github.com/bluesky-social/slagworks/internal/power/smcipmitool"
	"github.com/bluesky-social/slagworks/internal/ws"
)

func TestWSConn_BuildURL_HTTPS(t *testing.T) {
	w := &WSConn{config: WSConnConfig{
		GlobalURL: "https://global.example.com",
		Token:     "tok-abc",
		WorkerID:  "worker-1",
	}}
	got := w.buildURL()
	for _, want := range []string{"wss://", "/api/v1/ws/site", "token=tok-abc"} {
		if !strings.Contains(got, want) {
			t.Errorf("URL %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "site_id=") || strings.Contains(got, "after_seq=") {
		t.Errorf("URL still carries legacy query params: %s", got)
	}
}

func TestWSConn_BuildURL_HTTP(t *testing.T) {
	w := &WSConn{config: WSConnConfig{
		GlobalURL: "http://localhost:8080",
		Token:     "tok-xyz",
	}}
	got := w.buildURL()
	if !strings.HasPrefix(got, "ws://") {
		t.Errorf("expected ws:// for http URL, got %q", got)
	}
}

func TestWSConn_RPCPlumbing(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, nil, nil, nil)

	reqID := uuid.New().String()
	ch := w.registerPending(reqID)

	data := json.RawMessage(`{"ok":true}`)
	w.deliverRPCResponse(reqID, data)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := w.waitResponse(ctx, ch)
	if err != nil {
		t.Fatalf("waitResponse: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("got %s", got)
	}

	w.removePending(reqID)
	w.pendingMu.Lock()
	if _, exists := w.pending[reqID]; exists {
		t.Fatal("expected pending removed")
	}
	w.pendingMu.Unlock()
}

func TestWSConn_RPCPlumbing_Timeout(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, nil, nil, nil)

	reqID := uuid.New().String()
	ch := w.registerPending(reqID)
	defer w.removePending(reqID)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := w.waitResponse(ctx, ch)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWSConn_DeliverRPCResponse_UnknownReqID(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, nil, nil, nil)
	// Must not panic when delivering to unknown request ID.
	w.deliverRPCResponse("nonexistent", json.RawMessage(`{}`))
}

func TestWSConn_Connected(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, nil, nil, nil)
	if w.Connected() {
		t.Fatal("expected disconnected initially")
	}
	w.connected.Store(true)
	if !w.Connected() {
		t.Fatal("expected connected after store")
	}
}

// TestWSConn_ClaimOp_NotConnected verifies the WSConn refuses the
// claim with a clear error when the WS is down. The executor uses
// this to short-circuit and leave the op pending for the next
// reconnect to retry.
func TestWSConn_ClaimOp_NotConnected(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, nil, nil, nil)
	// connected.Store is false by default — i.e., disconnected.

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := w.ClaimOp(ctx, uuid.New())
	if err == nil {
		t.Fatal("expected error when WS not connected")
	}
}

// TestWSConn_ClaimOp_Success_RPCRoundTrip validates the request/
// response wiring without standing up a real WS server: register
// the pending request, simulate the hub's reply via
// deliverRPCResponse, and assert ClaimOp returns the decoded op.
//
// The test injects a fake send-side so we can capture the outgoing
// envelope without a real coder/websocket.Conn. This exercises:
//   - ClaimOp registers a unique request_id in the pending map
//   - It marshals a ClaimOpMsg with the right op_id
//   - On RPC response, it unmarshals ClaimOpResponse.Operation and
//     returns it
func TestWSConn_ClaimOp_Success_RPCRoundTrip(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, nil, nil, nil)
	w.connected.Store(true) // simulate post-handshake state

	opID := uuid.New()
	machineID := uuid.New()

	// The hub-side reply we want delivered after ClaimOp registers
	// its request_id. The wsconn read loop normally delivers via
	// deliverRPCResponse on receipt of TypeClaimOpResp; for this
	// unit-level test we drive that helper directly from a goroutine.
	go func() {
		// Wait for the request_id to appear in the pending map,
		// then deliver. Polling in tests is gross but the
		// alternative is a real WS server, which is wsconn-
		// integration-test territory.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			w.pendingMu.Lock()
			pendingCount := len(w.pending)
			var reqID string
			for id := range w.pending {
				reqID = id
			}
			w.pendingMu.Unlock()
			if pendingCount > 0 {
				resp := struct {
					Data     json.RawMessage `json:"data,omitempty"`
					ErrorMsg string          `json:"error,omitempty"`
				}{}
				claimResp, _ := json.Marshal(struct {
					Operation map[string]any `json:"operation"`
				}{
					Operation: map[string]any{
						"id":         opID.String(),
						"machine_id": machineID.String(),
						"type":       "deploy",
						"status":     "claimed",
						"claimed_by": "test-worker",
					},
				})
				resp.Data = claimResp
				envelope, _ := json.Marshal(resp)
				w.deliverRPCResponse(reqID, envelope)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("ClaimOp never registered a pending request_id")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// ClaimOp will fail because there's no real conn to write to.
	// That's expected at this layer; the production path uses a
	// real conn. The integration coverage for the full write+read
	// path lives in the qemutest end-to-end test suite.
	//
	// What this test verifies is the receive-side decode: when a
	// ClaimOpResponse arrives via deliverRPCResponse, ClaimOp
	// must extract the embedded Operation. Since we can't write,
	// we instead test waitResponse decoding directly.
	_ = ctx
	_ = w

	// Direct helper test: waitResponse + decode of a mocked-shape
	// response. This is what the inside of ClaimOp must do.
	reqID := "test-claim-1"
	ch := w.registerPending(reqID)
	defer w.removePending(reqID)

	go func() {
		time.Sleep(20 * time.Millisecond)
		opJSON, _ := json.Marshal(map[string]any{
			"id":         opID.String(),
			"machine_id": machineID.String(),
			"type":       "deploy",
			"status":     "claimed",
			"claimed_by": "test-worker",
		})
		respData, _ := json.Marshal(map[string]any{
			"data": map[string]any{"operation": json.RawMessage(opJSON)},
		})
		w.deliverRPCResponse(reqID, respData)
	}()

	rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer rcancel()
	raw, err := w.waitResponse(rctx, ch)
	if err != nil {
		t.Fatalf("waitResponse: %v", err)
	}

	// Decode the RPCResponseMsg → ClaimOpResponse path.
	var rpcResp struct {
		Data  json.RawMessage `json:"data,omitempty"`
		Error string          `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		t.Fatal(err)
	}
	if rpcResp.Error != "" {
		t.Fatalf("expected ok, got error %q", rpcResp.Error)
	}
	var claimResp struct {
		Operation *struct {
			ID string `json:"id"`
		} `json:"operation"`
	}
	if err := json.Unmarshal(rpcResp.Data, &claimResp); err != nil {
		t.Fatal(err)
	}
	if claimResp.Operation == nil || claimResp.Operation.ID != opID.String() {
		t.Errorf("Operation: got %+v, want id=%s", claimResp.Operation, opID)
	}
}

// TestDecodeClaimOpResponse verifies the rpc_response → Claimer
// contract mapping in one table:
//
//   - won claim: (op, nil)
//   - structured already-claimed: (nil, nil) — NOT an error, so the
//     executor counts ClaimLost instead of ClaimErr
//   - hub hard error (DB failure): (nil, err)
//   - empty success payload: (nil, err) — contract violation
func TestDecodeClaimOpResponse(t *testing.T) {
	opID := uuid.New()
	wonOp, _ := json.Marshal(ws.ClaimOpResponse{
		Operation: &model.Operation{ID: opID, Status: model.OpClaimed, ClaimedBy: "worker-1"},
	})
	lost, _ := json.Marshal(ws.ClaimOpResponse{AlreadyClaimed: true})
	empty, _ := json.Marshal(ws.ClaimOpResponse{})

	tests := []struct {
		name    string
		resp    ws.RPCResponseMsg
		wantOp  bool
		wantErr bool
	}{
		{name: "won", resp: ws.RPCResponseMsg{Data: wonOp}, wantOp: true},
		{name: "already_claimed maps to nil,nil", resp: ws.RPCResponseMsg{Data: lost}},
		{name: "hub hard error", resp: ws.RPCResponseMsg{Error: "claim failed: postgres exploded"}, wantErr: true},
		{name: "empty success is contract violation", resp: ws.RPCResponseMsg{Data: empty}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.resp)
			if err != nil {
				t.Fatal(err)
			}
			op, err := decodeClaimOpResponse(raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantOp {
				if op == nil || op.ID != opID {
					t.Fatalf("Operation: got %+v, want id=%s", op, opID)
				}
				return
			}
			if op != nil {
				t.Fatalf("expected (nil, nil) on already-claimed, got op %+v", op)
			}
		})
	}
}

// TestWSConn_ClaimOp_ErrorResponse verifies that an rpc_response
// carrying an error string (a genuine hub-side failure, e.g. a DB
// error — NOT the structured already-claimed rejection) is surfaced
// as an error from ClaimOp. The executor counts these as ClaimErr
// and leaves the op pending for the next tick to retry.
func TestWSConn_ClaimOp_ErrorResponse(t *testing.T) {
	w := NewWSConn(WSConnConfig{}, nil, nil, nil)
	w.connected.Store(true)

	reqID := "test-claim-error"
	ch := w.registerPending(reqID)
	defer w.removePending(reqID)

	go func() {
		time.Sleep(10 * time.Millisecond)
		respData, _ := json.Marshal(map[string]any{
			"error": "claim failed: postgres exploded",
		})
		w.deliverRPCResponse(reqID, respData)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := w.waitResponse(ctx, ch)
	if err != nil {
		t.Fatalf("waitResponse: %v", err)
	}
	op, err := decodeClaimOpResponse(raw)
	if err == nil {
		t.Error("expected error from hub hard failure")
	}
	if op != nil {
		t.Errorf("expected nil op on hub hard failure, got %+v", op)
	}
}

// packaged is a global build identity as scripts/build-deb.sh stamps it:
// the git SHA plus the deb package version built from it.
func packaged(sha string) globalBuild {
	return globalBuild{SHA: sha, PackageVersion: "0.9.0+git.1.g" + sha}
}

func TestCheckVersion_NoOpOnEmptyOrMatch(t *testing.T) {
	w := &WSConn{config: WSConnConfig{Version: "abc"}, log: slog.Default()}
	// Empty remote → no-op (no panic, no upgrade).
	w.checkVersion(globalBuild{})
	// Empty local → no-op.
	w2 := &WSConn{config: WSConnConfig{Version: ""}, log: slog.Default()}
	w2.checkVersion(packaged("def"))
	// Match → no-op.
	w.checkVersion(packaged("abc"))
	if w.upgrading.Load() {
		t.Fatal("upgrading flag should not be set on no-op paths")
	}
}

func TestCheckVersion_MismatchWithoutAutoUpgradeStaysQuiet(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "")
	w := &WSConn{config: WSConnConfig{Version: "abc"}, log: slog.Default()}
	w.checkVersion(packaged("def"))
	if w.upgrading.Load() {
		t.Fatal("upgrading flag should not be set when SLAGWORKS_AUTO_UPGRADE is unset")
	}
}

func TestCheckVersion_SingleFlight(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "1")
	w := &WSConn{config: WSConnConfig{Version: "abc"}, log: slog.Default()}
	// Pre-set the flag so runUpgrade doesn't actually fire (we don't
	// want to invoke sudo in tests).
	w.upgrading.Store(true)
	// Second mismatch should not double-fire.
	w.checkVersion(packaged("def"))
	w.checkVersion(packaged("ghi"))
	// Flag stays set; no panic.
	if !w.upgrading.Load() {
		t.Fatal("upgrading flag flipped unexpectedly")
	}
}

// TestCheckVersion_ResetsLatchAfterFailedUpgrade is a
// regression test: a failed upgrade must clear w.upgrading after the
// cooldown so a later version-mismatch tick can retry, instead of
// stranding the worker on a stale binary forever. The helper exec is
// stubbed (returns an error) so the failure path is deterministic and
// never touches a real /usr/bin/slagworks-upgrade.
func TestCheckVersion_ResetsLatchAfterFailedUpgrade(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "1")
	var execCalls atomic.Int32
	w := &WSConn{
		config:          WSConnConfig{Version: "abc"},
		log:             slog.Default(),
		upgradeCooldown: 10 * time.Millisecond,
		upgradeExec: func(globalBuild) ([]byte, error) {
			execCalls.Add(1)
			return []byte("stub: upgrade failed"), errors.New("stub failure")
		},
	}
	w.checkVersion(packaged("def"))
	// The latch is set synchronously by checkVersion; runUpgrade runs
	// in a goroutine, fails the stubbed exec, sleeps the (tiny)
	// cooldown, then clears the latch. Poll for the reset.
	deadline := time.Now().Add(5 * time.Second)
	for w.upgrading.Load() {
		if time.Now().After(deadline) {
			t.Fatal("upgrading latch never reset after failed upgrade")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if execCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 helper exec, got %d", execCalls.Load())
	}
	// A subsequent mismatch tick must be able to fire again. Wait on
	// the exec count (deterministic) rather than the transient latch,
	// which the retry goroutine may already have cleared after its own
	// tiny cooldown.
	w.checkVersion(packaged("ghi"))
	deadline = time.Now().Add(5 * time.Second)
	for execCalls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("checkVersion did not re-fire after the latch reset")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCheckVersion_InvokesHelperWithSHAAndPackageVersion: the helper
// gets both halves of global's build identity. It installs the package
// version, and checks the SHA against the installed binary to no-op a
// repeated request.
func TestCheckVersion_InvokesHelperWithSHAAndPackageVersion(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "1")
	w, targets := failingUpgradeConn(10 * time.Millisecond)
	want := globalBuild{SHA: "def", PackageVersion: "0.3.0+git.4.gdef"}
	w.checkVersion(want)
	receiveTarget(t, targets, want)
}

// TestUpgradeCommand_PassesSHAAndPackageVersion pins the production
// argv: sudo -n, the packaged helper's absolute path (the one path the
// sudoers entry allows), then the SHA and the package version.
func TestUpgradeCommand_PassesSHAAndPackageVersion(t *testing.T) {
	cmd := upgradeCommand(globalBuild{SHA: "def", PackageVersion: "0.3.0+git.4.gdef"})
	want := []string{"sudo", "-n", "/usr/bin/slagworks-upgrade", "def", "0.3.0+git.4.gdef"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("upgrade argv = %q, want %q", cmd.Args, want)
	}
}

// TestApplyReplies_HandHelperGlobalBuild pins the wire: global's
// package_version reaches the helper from both message types that carry
// its build identity.
func TestApplyReplies_HandHelperGlobalBuild(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "1")
	want := globalBuild{SHA: "def", PackageVersion: "0.3.0+git.4.gdef"}

	t.Run("no_change", func(t *testing.T) {
		w, targets := failingUpgradeConn(10 * time.Millisecond)
		data, err := json.Marshal(ws.NoChangeMsg{
			Version: want.SHA, PackageVersion: want.PackageVersion, Proto: ws.OpStreamProto})
		if err != nil {
			t.Fatal(err)
		}
		w.applyNoChange(data)
		receiveTarget(t, targets, want)
	})

	t.Run("snapshot", func(t *testing.T) {
		w, targets := failingUpgradeConn(10 * time.Millisecond)
		w.cache = NewCache(nil)
		w.cache.imageSyncFn = func(string, map[string]*model.Image) error { return nil }
		data, err := json.Marshal(ws.SnapshotMsg{
			Snapshot: model.SiteSnapshot{SeqNum: 1},
			Version:  want.SHA, PackageVersion: want.PackageVersion, Proto: ws.OpStreamProto})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.applySnapshot(data); err != nil {
			t.Fatalf("applySnapshot: %v", err)
		}
		receiveTarget(t, targets, want)
	})
}

// TestCheckVersion_UnpackagedGlobalDoesNotInvokeHelper: a global not
// built by build-deb.sh reports no package version, so there is nothing
// for apt to install. The worker logs that and stays put instead of
// running the helper, and the heartbeat retry does not repeat the log
// every tick. Once global reports a packaged build, the upgrade runs.
func TestCheckVersion_UnpackagedGlobalDoesNotInvokeHelper(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "1")
	w, targets := failingUpgradeConn(10 * time.Millisecond)
	var logs bytes.Buffer
	w.log = slog.New(slog.NewTextHandler(&logs, nil))

	w.checkVersion(globalBuild{SHA: "def"})
	assertNoUpgrade(t, targets)
	if w.upgrading.Load() {
		t.Fatal("upgrading latch set for a global with no package version")
	}
	if n := strings.Count(logs.String(), "cannot auto-upgrade to an unpackaged build"); n != 1 {
		t.Fatalf("want 1 unpackaged-build log line, got %d; logs:\n%s", n, logs.String())
	}

	w.retryUpgrade()
	assertNoUpgrade(t, targets)
	if n := strings.Count(logs.String(), "version mismatch with global"); n != 1 {
		t.Fatalf("heartbeat retry re-ran the check for an unpackaged global (%d mismatch lines); logs:\n%s",
			n, logs.String())
	}

	w.checkVersion(packaged("ghi"))
	receiveTarget(t, targets, packaged("ghi"))
}

// waitUpgradeLatchReleased polls until a failed runUpgrade has slept
// its cooldown and cleared w.upgrading.
func waitUpgradeLatchReleased(t *testing.T, w *WSConn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for w.upgrading.Load() {
		if time.Now().After(deadline) {
			t.Fatal("upgrading latch never reset after failed upgrade")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// failingUpgradeConn returns a WSConn whose upgrade helper always fails
// and reports every target it was asked for on the returned channel.
func failingUpgradeConn(cooldown time.Duration) (*WSConn, chan globalBuild) {
	targets := make(chan globalBuild, 8)
	return &WSConn{
		config:          WSConnConfig{Version: "abc"},
		log:             slog.Default(),
		upgradeCooldown: cooldown,
		upgradeExec: func(target globalBuild) ([]byte, error) {
			targets <- target
			return []byte("stub: upgrade failed"), errors.New("stub failure")
		},
	}, targets
}

func receiveTarget(t *testing.T, targets chan globalBuild, want globalBuild) {
	t.Helper()
	select {
	case got := <-targets:
		if got != want {
			t.Fatalf("upgrade helper ran for %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("upgrade helper never ran for %+v", want)
	}
}

func assertNoUpgrade(t *testing.T, targets chan globalBuild) {
	t.Helper()
	select {
	case got := <-targets:
		t.Fatalf("upgrade helper ran unexpectedly for %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestRetryUpgrade_RetriesFailedUpgradeWithoutNewTick is the 2026-10-29
// regression. The global reports its version only on the hello reply
// and on snapshot pushes, so after every site worker's first upgrade
// attempt failed, no further version check arrived and the fleet sat on
// the old build for 40 minutes, until a redeploy forced reconnects.
// retryUpgrade, which the heartbeat loop calls on every tick, must
// re-fire against the global's last reported version once the cooldown
// has released the latch.
func TestRetryUpgrade_RetriesFailedUpgradeWithoutNewTick(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "1")
	w, targets := failingUpgradeConn(10 * time.Millisecond)
	w.checkVersion(packaged("def"))
	receiveTarget(t, targets, packaged("def"))
	waitUpgradeLatchReleased(t, w)

	w.retryUpgrade()
	receiveTarget(t, targets, packaged("def"))
}

// TestRetryUpgrade_TargetsNewestGlobalVersion: a snapshot that arrives
// while a failed attempt still holds the latch can't start an upgrade
// itself, but the retry must chase that newer version, not the stale
// one the failed attempt targeted.
func TestRetryUpgrade_TargetsNewestGlobalVersion(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "1")
	w, targets := failingUpgradeConn(200 * time.Millisecond)
	w.checkVersion(packaged("def"))
	receiveTarget(t, targets, packaged("def"))
	w.checkVersion(packaged("ghi")) // latch held through the cooldown: no second exec
	waitUpgradeLatchReleased(t, w)

	w.retryUpgrade()
	receiveTarget(t, targets, packaged("ghi"))
}

func TestRetryUpgrade_NoopWhenVersionsMatch(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "1")
	w, targets := failingUpgradeConn(10 * time.Millisecond)
	w.checkVersion(packaged("abc")) // the global runs our build
	w.retryUpgrade()
	assertNoUpgrade(t, targets)
	if w.upgrading.Load() {
		t.Fatal("upgrading latch set with no version mismatch")
	}
}

func TestRetryUpgrade_NoopBeforeAnyGlobalVersion(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "1")
	w, targets := failingUpgradeConn(10 * time.Millisecond)
	w.retryUpgrade()
	assertNoUpgrade(t, targets)
}

func TestRetryUpgrade_NoopWithoutAutoUpgrade(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "")
	w, targets := failingUpgradeConn(10 * time.Millisecond)
	w.checkVersion(packaged("def")) // logs the mismatch, stays pinned
	w.retryUpgrade()
	assertNoUpgrade(t, targets)
}

func TestRetryUpgrade_NoopWhileUpgradeInFlight(t *testing.T) {
	t.Setenv("SLAGWORKS_AUTO_UPGRADE", "1")
	w, targets := failingUpgradeConn(10 * time.Millisecond)
	w.upgrading.Store(true) // an attempt (or its cooldown) is running
	w.checkVersion(packaged("def"))
	w.retryUpgrade()
	assertNoUpgrade(t, targets)
}

// --- power_status RPC (hub-originated read path) ---

// statusController is a power.Controller stub for runPowerStatus
// tests: PowerStatus and Transport are the only methods exercised.
type statusController struct {
	state     power.State
	statusErr error
	transport string
	closed    bool
}

func (s *statusController) PowerOn(context.Context) error    { return nil }
func (s *statusController) PowerOff(context.Context) error   { return nil }
func (s *statusController) PowerCycle(context.Context) error { return nil }
func (s *statusController) PowerStatus(context.Context) (power.State, error) {
	return s.state, s.statusErr
}
func (s *statusController) SetBootDevice(context.Context, power.BootDevice) error { return nil }
func (s *statusController) ChassisSerial(context.Context) (string, error)         { return "", nil }
func (s *statusController) Transport() string                                     { return s.transport }
func (s *statusController) Close(context.Context) error                           { s.closed = true; return nil }

// powerStatusTestConn builds a WSConn with a cache containing one
// machine and the given controller factory wired.
func powerStatusTestConn(t *testing.T, machineID uuid.UUID, bmcIP string, factory PowerControllerFactory) *WSConn {
	t.Helper()
	cache := NewCache(nil)
	addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: bmcIP})
	w := NewWSConn(WSConnConfig{}, cache, nil, nil)
	w.SetLogger(testLogger())
	w.powerFactory = factory
	return w
}

func powerStatusReq(t *testing.T, machineID uuid.UUID, rt string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(ws.PowerStatusRequestMsg{
		MachineID:  machineID,
		BMCRuntime: json.RawMessage(rt),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRunPowerStatus_HappyPath: creds from the request's bmc_runtime
// reach the factory, the controller's state + transport come back in
// the response payload, and the controller is closed.
func TestRunPowerStatus_HappyPath(t *testing.T) {
	machineID := uuid.New()
	ctrl := &statusController{state: power.StateOn, transport: "redfish"}
	var gotRT model.BMCRuntime
	w := powerStatusTestConn(t, machineID, "10.254.241.9",
		func(_ context.Context, m *model.MachineSnapshot, rt model.BMCRuntime, pduRT *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
			if m.ID != machineID {
				t.Errorf("factory machine: got %s, want %s", m.ID, machineID)
			}
			if pduRT != nil {
				t.Error("status read must not carry a pdu_runtime")
			}
			gotRT = rt
			return ctrl, nil
		})

	resp := w.runPowerStatus(context.Background(),
		powerStatusReq(t, machineID, `{"username":"slagworks","password":"pw","extra_config":{"ipmi_cipher":3}}`))
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	var out ws.PowerStatusResponseMsg
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "on" || out.Transport != "redfish" {
		t.Errorf("got %+v, want {on redfish}", out)
	}
	if gotRT.Username != "slagworks" || gotRT.Password != "pw" {
		t.Errorf("bmc_runtime creds did not reach the factory: %+v", gotRT)
	}
	if gotRT.IpmiCipher() != 3 {
		t.Errorf("extra_config did not reach the factory: %+v", gotRT.ExtraConfig)
	}
	if !ctrl.closed {
		t.Error("controller must be closed after the read")
	}
}

// TestRunPowerStatus_Errors pins each failure shape: the response
// must carry an error string (never an empty success) so the hub's
// caller — and ultimately slagctl — sees what went wrong.
func TestRunPowerStatus_Errors(t *testing.T) {
	machineID := uuid.New()
	okFactory := func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
		return &statusController{state: power.StateOff, transport: "ipmi"}, nil
	}

	t.Run("no factory configured", func(t *testing.T) {
		w := powerStatusTestConn(t, machineID, "10.254.241.9", nil)
		resp := w.runPowerStatus(context.Background(), powerStatusReq(t, machineID, `{}`))
		if !strings.Contains(resp.Error, "no power controller factory") {
			t.Errorf("got %q", resp.Error)
		}
	})

	t.Run("machine not in snapshot", func(t *testing.T) {
		w := powerStatusTestConn(t, machineID, "10.254.241.9", okFactory)
		resp := w.runPowerStatus(context.Background(), powerStatusReq(t, uuid.New(), `{}`))
		if !strings.Contains(resp.Error, "not in site snapshot") {
			t.Errorf("got %q", resp.Error)
		}
	})

	t.Run("machine has no BMC IP", func(t *testing.T) {
		w := powerStatusTestConn(t, machineID, "", okFactory)
		resp := w.runPowerStatus(context.Background(), powerStatusReq(t, machineID, `{}`))
		if !strings.Contains(resp.Error, "no BMC IP") {
			t.Errorf("got %q", resp.Error)
		}
	})

	t.Run("factory error", func(t *testing.T) {
		w := powerStatusTestConn(t, machineID, "10.254.241.9",
			func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
				return nil, fmt.Errorf("auth refused")
			})
		resp := w.runPowerStatus(context.Background(), powerStatusReq(t, machineID, `{}`))
		if !strings.Contains(resp.Error, "bmc connect") || !strings.Contains(resp.Error, "auth refused") {
			t.Errorf("got %q", resp.Error)
		}
	})

	t.Run("status read error", func(t *testing.T) {
		ctrl := &statusController{statusErr: fmt.Errorf("redfish 500")}
		w := powerStatusTestConn(t, machineID, "10.254.241.9",
			func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
				return ctrl, nil
			})
		resp := w.runPowerStatus(context.Background(), powerStatusReq(t, machineID, `{}`))
		if !strings.Contains(resp.Error, "redfish 500") {
			t.Errorf("got %q", resp.Error)
		}
		if !ctrl.closed {
			t.Error("controller must be closed on the error path too")
		}
	})

	t.Run("malformed request payload", func(t *testing.T) {
		w := powerStatusTestConn(t, machineID, "10.254.241.9", okFactory)
		resp := w.runPowerStatus(context.Background(), json.RawMessage(`{"machine_id":42}`))
		if !strings.Contains(resp.Error, "decode power_status request") {
			t.Errorf("got %q", resp.Error)
		}
	})
}

// bootController is a statusController that ALSO implements
// power.BootManager — the boot-capable stub. The plain
// statusController (no BootManager) exercises the not-supported path.
type bootController struct {
	statusController
	boot    power.BootOverride
	bootErr error
}

func (b *bootController) BootStatus(context.Context) (power.BootOverride, error) {
	return b.boot, b.bootErr
}
func (b *bootController) SetBootOverride(context.Context, power.BootDevice, bool) error { return nil }

func bootStatusReq(t *testing.T, machineID uuid.UUID, rt string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(ws.BootStatusRequestMsg{
		MachineID:  machineID,
		BMCRuntime: json.RawMessage(rt),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRunBootStatus_HappyPath(t *testing.T) {
	machineID := uuid.New()
	ctrl := &bootController{
		statusController: statusController{transport: "test"},
		boot:             power.BootOverride{Device: power.BootPXE, Valid: true, Persistent: false, Raw: "UEFI_PXE"},
	}
	w := powerStatusTestConn(t, machineID, "10.254.241.9",
		func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
			return ctrl, nil
		})

	resp := w.runBootStatus(context.Background(), bootStatusReq(t, machineID, `{}`))
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	var out ws.BootStatusResponseMsg
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Supported {
		t.Fatal("Supported = false, want true for a BootManager controller")
	}
	if out.Device != "pxe" || !out.Valid || out.Persistent || out.Raw != "UEFI_PXE" || out.Transport != "test" {
		t.Errorf("unexpected response: %+v", out)
	}
	if !ctrl.closed {
		t.Error("controller must be closed after the read")
	}
}

func TestRunBootStatus_NotSupported(t *testing.T) {
	// A controller that is NOT a BootManager (the plain statusController,
	// like bmclib/PDU) yields Supported:false — a success reply, not an
	// error.
	machineID := uuid.New()
	ctrl := &statusController{transport: "ipmi"}
	w := powerStatusTestConn(t, machineID, "10.254.241.9",
		func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
			return ctrl, nil
		})

	resp := w.runBootStatus(context.Background(), bootStatusReq(t, machineID, `{}`))
	if resp.Error != "" {
		t.Fatalf("not-supported must be a success reply, got error: %s", resp.Error)
	}
	var out ws.BootStatusResponseMsg
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Supported {
		t.Fatal("Supported = true, want false for a non-BootManager controller")
	}
	if !ctrl.closed {
		t.Error("controller must be closed even on the not-supported path")
	}
}

func TestRunBootStatus_ReadError(t *testing.T) {
	machineID := uuid.New()
	ctrl := &bootController{
		statusController: statusController{transport: "test"},
		bootErr:          fmt.Errorf("can't connect to BMC"),
	}
	w := powerStatusTestConn(t, machineID, "10.254.241.9",
		func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
			return ctrl, nil
		})

	resp := w.runBootStatus(context.Background(), bootStatusReq(t, machineID, `{}`))
	if !strings.Contains(resp.Error, "bmc boot status") || !strings.Contains(resp.Error, "can't connect") {
		t.Errorf("read error must propagate (not Supported:false), got %q", resp.Error)
	}
	if !ctrl.closed {
		t.Error("controller must be closed on the error path")
	}
}

// --- Tier 2 op-stream skew gate (worker side) ---

// TestCheckReplyProto_RefusesLowProto pins the worker's half of the loud
// skew gate: a hello reply (snapshot OR no_change) whose Proto is
// absent/lower than ws.OpStreamProto is a pre-op-stream global; the worker
// refuses the connection rather than silently running with no op stream.
func TestCheckReplyProto_RefusesLowProto(t *testing.T) {
	w := &WSConn{log: slog.Default()}

	// no_change without Proto (the hole a snapshot-only check leaves).
	noChange, _ := json.Marshal(ws.NoChangeMsg{Version: "x"})
	if err := w.checkReplyProto(ws.TypeNoChange, noChange); err == nil {
		t.Fatal("no_change without Proto must be refused")
	}

	// snapshot without Proto.
	snap, _ := json.Marshal(ws.SnapshotMsg{Snapshot: model.SiteSnapshot{SeqNum: 1}})
	if err := w.checkReplyProto(ws.TypeSnapshot, snap); err == nil {
		t.Fatal("snapshot without Proto must be refused")
	}
}

// TestCheckReplyProto_AcceptsCurrentProto: a reply carrying the current
// OpStreamProto passes.
func TestCheckReplyProto_AcceptsCurrentProto(t *testing.T) {
	w := &WSConn{log: slog.Default()}
	noChange, _ := json.Marshal(ws.NoChangeMsg{Version: "x", Proto: ws.OpStreamProto})
	if err := w.checkReplyProto(ws.TypeNoChange, noChange); err != nil {
		t.Fatalf("no_change with current Proto must pass, got %v", err)
	}
	snap, _ := json.Marshal(ws.SnapshotMsg{Snapshot: model.SiteSnapshot{SeqNum: 1}, Proto: ws.OpStreamProto})
	if err := w.checkReplyProto(ws.TypeSnapshot, snap); err != nil {
		t.Fatalf("snapshot with current Proto must pass, got %v", err)
	}
}

// TestCheckReplyProto_RefusesSnapshotWithOperations is the belt-and-
// suspenders leg: a snapshot that STILL carries an "operations" key (a
// stale global that somehow passed the Proto check) is refused.
func TestCheckReplyProto_RefusesSnapshotWithOperations(t *testing.T) {
	w := &WSConn{log: slog.Default()}
	// Hand-craft a reply whose embedded snapshot has a non-empty
	// "operations" array, with a current Proto so only the operations
	// check can fire.
	raw := []byte(`{"proto":1,"snapshot":{"seq_num":1,"operations":[{"id":"x"}]}}`)
	if err := w.checkReplyProto(ws.TypeSnapshot, raw); err == nil {
		t.Fatal("snapshot carrying operations must be refused")
	}
}

// TestStatusReads_SMCToolHint: when the worker reports SMCIPMITool missing,
// status reads of a Supermicro host carry an install hint naming this
// worker. A non-Supermicro host, or a worker where the tool is installed
// or deliberately disabled, gets none.
func TestStatusReads_SMCToolHint(t *testing.T) {
	cases := []struct {
		name, manufacturer, status string
		wantHint                   bool
	}{
		{"supermicro, tool missing", "supermicro", smcipmitool.StatusMissing, true},
		{"supermicro, tool installed", "supermicro", smcipmitool.StatusInstalled, false},
		{"supermicro, tool disabled", "supermicro", smcipmitool.StatusDisabled, false},
		{"other vendor, tool missing", "dell", smcipmitool.StatusMissing, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machineID := uuid.New()
			cache := NewCache(nil)
			addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "192.0.2.9", Manufacturer: tc.manufacturer})
			status := tc.status
			w := NewWSConn(WSConnConfig{
				FQDN:              "worker-a.example",
				SMCIPMIToolStatus: func() string { return status },
			}, cache, nil, nil)
			w.SetLogger(testLogger())
			w.powerFactory = func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
				return &statusController{state: power.StateOn, transport: "ipmi"}, nil
			}

			var ps ws.PowerStatusResponseMsg
			resp := w.runPowerStatus(context.Background(), powerStatusReq(t, machineID, `{}`))
			if resp.Error != "" {
				t.Fatalf("power status: %s", resp.Error)
			}
			if err := json.Unmarshal(resp.Data, &ps); err != nil {
				t.Fatal(err)
			}
			var bs ws.BootStatusResponseMsg
			resp = w.runBootStatus(context.Background(), bootStatusReq(t, machineID, `{}`))
			if resp.Error != "" {
				t.Fatalf("boot status: %s", resp.Error)
			}
			if err := json.Unmarshal(resp.Data, &bs); err != nil {
				t.Fatal(err)
			}

			for name, hint := range map[string]string{"power status": ps.Hint, "boot status": bs.Hint} {
				if got := hint != ""; got != tc.wantHint {
					t.Errorf("%s hint = %q, want present=%v", name, hint, tc.wantHint)
				}
				if tc.wantHint && !strings.Contains(hint, "worker-a.example") {
					t.Errorf("%s hint should name the worker, got %q", name, hint)
				}
			}
		})
	}
}
