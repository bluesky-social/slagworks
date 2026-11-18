package cmd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/api"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// TestDHCPLeases_RendersTable pins the table shape for the happy path:
// leases from two workers, sorted by IP, with the machine label
// falling back correctly.
func TestDHCPLeases_RendersTable(t *testing.T) {
	resp := api.DHCPLeasesResponse{
		Workers: []api.DHCPLeasesWorkerResult{
			{
				WorkerID: "worker-2",
				FQDN:     "worker-2.pop1",
				Leases: []ws.DHCPLeaseInfo{
					{MAC: "aa:bb:cc:00:00:02", IP: "10.99.1.200", Kind: "dynamic", Pool: "10.99.1.0/24",
						ExpiresAt: time.Now().Add(5 * time.Minute)},
				},
			},
			{
				WorkerID: "worker-1",
				FQDN:     "worker-1.pop1",
				Leases: []ws.DHCPLeaseInfo{
					{MAC: "aa:bb:cc:00:00:01", IP: "10.99.1.9", Kind: "static", Pool: "10.99.1.0/24",
						MachineID: "abc-123", Hostname: "kworker-1.pop1",
						ExpiresAt: time.Now().Add(time.Hour)},
				},
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs/pop1/dhcp-leases", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "dhcp", "leases", "pop1")
	if err != nil {
		t.Fatal(err)
	}
	// Numeric IP sort: .9 must come before .200 despite the lexical order.
	idx9 := strings.Index(out, "10.99.1.9")
	idx200 := strings.Index(out, "10.99.1.200")
	if idx9 == -1 || idx200 == -1 || idx9 > idx200 {
		t.Errorf("expected 10.99.1.9 before 10.99.1.200 (numeric IP sort), got:\n%s", out)
	}
	if !strings.Contains(out, "kworker-1.pop1") {
		t.Errorf("expected hostname in output, got:\n%s", out)
	}
	if !strings.Contains(out, "worker-2.pop1") {
		t.Errorf("expected worker fqdn in output, got:\n%s", out)
	}
}

// TestDHCPLeases_WorkerErrorRendersAsWarningNotFatal pins that a
// per-worker error surfaces as a warning line, not a command failure —
// the other worker's leases must still print.
func TestDHCPLeases_WorkerErrorRendersAsWarningNotFatal(t *testing.T) {
	resp := api.DHCPLeasesResponse{
		Workers: []api.DHCPLeasesWorkerResult{
			{WorkerID: "worker-1", FQDN: "worker-1.pop1", Error: "site worker has no DHCP handler configured"},
			{WorkerID: "worker-2", FQDN: "worker-2.pop1", Leases: []ws.DHCPLeaseInfo{
				{MAC: "aa:bb:cc:00:00:02", IP: "10.99.1.200", Kind: "dynamic", Pool: "10.99.1.0/24"},
			}},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs/pop1/dhcp-leases", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "dhcp", "leases", "pop1")
	if err != nil {
		t.Fatalf("a per-worker error must not fail the command: %v", err)
	}
	if !strings.Contains(out, "no DHCP handler") {
		t.Errorf("expected the worker error as a warning line, got:\n%s", out)
	}
	if !strings.Contains(out, "10.99.1.200") {
		t.Errorf("healthy worker's leases must still print, got:\n%s", out)
	}
}

// TestDHCPLeases_JSONPassthrough pins that --json emits the raw
// response shape rather than the table renderer's output.
func TestDHCPLeases_JSONPassthrough(t *testing.T) {
	resp := api.DHCPLeasesResponse{Workers: []api.DHCPLeasesWorkerResult{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs/pop1/dhcp-leases", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "dhcp", "leases", "pop1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got api.DHCPLeasesResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output did not parse as DHCPLeasesResponse: %v\noutput: %s", err, out)
	}
}

// TestDHCPLeases_NoWorkersConnected pins the empty-cage message.
func TestDHCPLeases_NoWorkersConnected(t *testing.T) {
	resp := api.DHCPLeasesResponse{Workers: []api.DHCPLeasesWorkerResult{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs/pop1/dhcp-leases", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "dhcp", "leases", "pop1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no site workers connected") {
		t.Errorf("expected a no-workers message, got:\n%s", out)
	}
}

// TestDHCPHistory_RendersTable pins the table shape: an open row and
// a superseded (closed) row, with the machine label falling back to a
// shortened UUID for a row with no hostname (the ledger carries none).
func TestDHCPHistory_RendersTable(t *testing.T) {
	machineID := uuid.New()
	now := time.Now()
	resp := api.DHCPLeaseHistoryResponse{
		Entries: []model.DHCPLeaseLedgerEntry{
			{
				MAC: "aa:bb:cc:00:00:01", IP: "10.99.1.100", Kind: "dynamic", Pool: "10.99.1.0/24",
				WorkerID: "worker-1", MachineID: &machineID,
				FirstSeen: now.Add(-time.Hour), LastSeen: now, ExpiresAt: now.Add(5 * time.Minute),
			},
			{
				MAC: "aa:bb:cc:00:00:02", IP: "10.99.1.101", Kind: "dynamic", Pool: "10.99.1.0/24",
				WorkerID:  "worker-1",
				FirstSeen: now.Add(-2 * time.Hour), LastSeen: now.Add(-time.Hour), ExpiresAt: now.Add(-30 * time.Minute),
				ReleasedAt: timePtr(now.Add(-time.Hour)), ReleaseReason: "superseded",
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs/pop1/dhcp-lease-history", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "dhcp", "history", "pop1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "10.99.1.100") || !strings.Contains(out, "10.99.1.101") {
		t.Errorf("expected both IPs in output, got:\n%s", out)
	}
	if !strings.Contains(out, "superseded") {
		t.Errorf("expected the closed row's status, got:\n%s", out)
	}
	if !strings.Contains(out, "open") {
		t.Errorf("expected the open row's status, got:\n%s", out)
	}
	if !strings.Contains(out, "worker-1") {
		t.Errorf("expected worker attribution, got:\n%s", out)
	}
}

// TestDHCPHistory_UnresolvedMachineShowsDash pins that a row with no
// MachineID renders "-" rather than a blank/panicking column.
func TestDHCPHistory_UnresolvedMachineShowsDash(t *testing.T) {
	now := time.Now()
	resp := api.DHCPLeaseHistoryResponse{
		Entries: []model.DHCPLeaseLedgerEntry{
			{
				MAC: "aa:bb:cc:00:00:03", IP: "10.99.1.102", Kind: "dynamic", Pool: "10.99.1.0/24",
				WorkerID:  "worker-1",
				FirstSeen: now, LastSeen: now, ExpiresAt: now.Add(5 * time.Minute),
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs/pop1/dhcp-lease-history", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "dhcp", "history", "pop1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "-") {
		t.Errorf("expected a dash for the unresolved machine column, got:\n%s", out)
	}
}

// TestDHCPHistory_JSONPassthrough pins that --json emits the raw
// response shape rather than the table renderer's output.
func TestDHCPHistory_JSONPassthrough(t *testing.T) {
	resp := api.DHCPLeaseHistoryResponse{Entries: []model.DHCPLeaseLedgerEntry{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs/pop1/dhcp-lease-history", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "dhcp", "history", "pop1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got api.DHCPLeaseHistoryResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output did not parse as DHCPLeaseHistoryResponse: %v\noutput: %s", err, out)
	}
}

// TestDHCPHistory_EmptyHistory pins the empty-history message.
func TestDHCPHistory_EmptyHistory(t *testing.T) {
	resp := api.DHCPLeaseHistoryResponse{Entries: []model.DHCPLeaseLedgerEntry{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs/pop1/dhcp-lease-history", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "dhcp", "history", "pop1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no lease history") {
		t.Errorf("expected a no-history message, got:\n%s", out)
	}
}

// TestDHCPHistory_FiltersPassedAsQueryParams pins that --mac/--ip/
// --limit reach the server as query params.
func TestDHCPHistory_FiltersPassedAsQueryParams(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs/pop1/dhcp-lease-history", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(api.DHCPLeaseHistoryResponse{Entries: []model.DHCPLeaseLedgerEntry{}})
	})
	cl := testServer(t, mux)
	if _, err := execCmd(t, cl, "dhcp", "history", "pop1", "--mac", "aa:bb:cc:00:00:01", "--ip", "10.99.1.100", "--limit", "5"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "mac=aa%3Abb%3Acc%3A00%3A00%3A01") {
		t.Errorf("expected mac in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "ip=10.99.1.100") {
		t.Errorf("expected ip in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "limit=5") {
		t.Errorf("expected limit in query, got %q", gotQuery)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
