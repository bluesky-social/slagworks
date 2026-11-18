//go:build integration

package qemutest

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// subnetPool hands out non-overlapping /24 subnets from the
// 198.18.0.0/15 IANA benchmarking range (RFC 2544/6815). That range
// is never routed legitimately on our hosts, so tests can't
// accidentally hijack traffic by picking an in-use subnet.
//
// Layout: 512 /24 slots indexed 0..511. Index i decodes to
//
//	198.(18 + i/256).(i % 256).0/24
//
// so the full 9-bit space is used without invalid-octet arithmetic.
//
// A random starting index is chosen once per process, and per-test
// allocations increment from there with wrap-modulo-512. This means
// two concurrent suite invocations pick independent
// starts; collision requires the second starting within
// `tests_in_flight` positions of the first. Each test's
// bridge lives in its own netns, so even a subnet overlap between
// concurrent invocations is harmless (no shared L2, no runtime
// detection — the randomized start just keeps the odds low).
type subnetPool struct {
	start uint16 // 0..511
	seq   atomic.Int32
	runID string
}

var (
	globalPool     *subnetPool
	globalPoolOnce sync.Once
)

const poolSize = 512

func newSubnetPool() *subnetPool {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	start := binary.BigEndian.Uint16(b[:]) % poolSize
	return &subnetPool{
		start: start,
		runID: uuid.New().String(),
	}
}

func getPool() *subnetPool {
	globalPoolOnce.Do(func() {
		globalPool = newSubnetPool()
	})
	return globalPool
}

// Allocate returns a fresh (gateway, subnet) pair. Fails the test if
// all 512 slots have been consumed by this invocation.
func (p *subnetPool) Allocate(t *testing.T) (gateway, subnet string) {
	t.Helper()
	next := p.seq.Add(1)
	if next > poolSize {
		t.Fatalf("qemutest: subnet pool exhausted (%d > %d)", next, poolSize)
	}
	i := (int(p.start) + int(next) - 1) % poolSize
	third := 18 + i/256
	fourth := i % 256
	gateway = fmt.Sprintf("198.%d.%d.1", third, fourth)
	subnet = fmt.Sprintf("198.%d.%d.0/24", third, fourth)
	return gateway, subnet
}

// RunID returns the per-process identifier. Bridge names hash this
// plus the test name so concurrent invocations don't collide even if
// they hit the same slot.
func (p *subnetPool) RunID() string {
	return p.runID
}
