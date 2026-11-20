package inventory

import (
	"fmt"
	"strings"
	"testing"
)

func lscpuJSON(sockets string) string {
	return fmt.Sprintf(`{"lscpu":[{"field":"Model name:","data":"AMD EPYC 9654 96-Core Processor"},`+
		`{"field":"Socket(s):","data":%q},{"field":"Core(s) per socket:","data":"96"},{"field":"Thread(s) per core:","data":"2"}]}`, sockets)
}

// TestCollectCPUs_SocketCountBounded: collectCPUs makes one entry per
// socket lscpu reports. A count no machine has (corrupted output, a broken
// lscpu) must come back as an error naming the count, not as an allocation
// of that many entries: a count in the billions took the agent down with an
// out-of-memory, and a larger one panicked in make. The largest count
// accepted still yields one entry per socket.
func TestCollectCPUs_SocketCountBounded(t *testing.T) {
	for _, sockets := range []string{"1025", "4294967296", "9223372036854775807"} {
		r := newMapRunner()
		r.set(lscpuJSON(sockets), "lscpu", "--json")
		cpus, err := collectCPUs(r)
		if err == nil {
			t.Fatalf("Socket(s) = %s: want an error, got %d CPU entries", sockets, len(cpus))
		}
		if len(cpus) != 0 {
			t.Errorf("Socket(s) = %s: an error must come with no CPU entries, got %d", sockets, len(cpus))
		}
		for _, want := range []string{sockets, "1024", "lscpu"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Socket(s) = %s: error should mention %q: %v", sockets, want, err)
			}
		}
	}

	r := newMapRunner()
	r.set(lscpuJSON("1024"), "lscpu", "--json")
	cpus, err := collectCPUs(r)
	if err != nil || len(cpus) != 1024 {
		t.Fatalf("Socket(s) = 1024, the bound: got %d entries, err %v", len(cpus), err)
	}
	if cpus[1023].Cores != 96 || cpus[1023].Threads != 192 {
		t.Errorf("last entry: %+v", cpus[1023])
	}
}

// TestCollect_AbsurdSocketCountFailsTheStep: the commission's lscpu step
// reports the refusal as its error instead of "no CPU info".
func TestCollect_AbsurdSocketCountFailsTheStep(t *testing.T) {
	r := newMapRunner()
	r.set(lscpuJSON("4294967296"), "lscpu", "--json")
	res := &CommissionResult{}
	v := lscpuStep(r, res)
	if v.Status != StepStatusFailed || !strings.Contains(v.Error, "4294967296") {
		t.Errorf("verdict = %+v, want failed with the socket count", v)
	}
	if len(res.CPUs) != 0 {
		t.Errorf("no CPU entries may be recorded, got %d", len(res.CPUs))
	}
}
