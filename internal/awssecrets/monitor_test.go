package awssecrets

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestFakeMonitorWriter_FreshPop covers cold start: the pop's secret
// map didn't exist; upsert creates it with the single entry.
func TestFakeMonitorWriter_FreshPop(t *testing.T) {
	w := &FakeMonitorWriter{}
	entry := BMCMonitorEntry{
		Kind:        "bmc",
		Username:    "monitor",
		Password:    "ABCDEFGHIJ",
		ExtraConfig: map[string]any{},
	}
	if err := w.UpsertMonitorEntry(context.Background(), "pop1", "SN-HOST1", entry); err != nil {
		t.Fatalf("UpsertMonitorEntry: %v", err)
	}
	got, ok := w.Secrets["pop1"]["SN-HOST1"]
	if !ok {
		t.Fatal("expected SN-HOST1 entry in pop1")
	}
	if got.Kind != entry.Kind || got.Username != entry.Username || got.Password != entry.Password {
		t.Errorf("entry round-trip: got %+v, want %+v", got, entry)
	}
}

// TestFakeMonitorWriter_ReplaceExisting verifies that upserting an
// existing serial replaces (not duplicates) the entry.
func TestFakeMonitorWriter_ReplaceExisting(t *testing.T) {
	w := &FakeMonitorWriter{}
	first := BMCMonitorEntry{Kind: "bmc", Username: "monitor", Password: "OLD0000000"}
	second := BMCMonitorEntry{Kind: "bmc", Username: "monitor", Password: "NEW0000000"}
	if err := w.UpsertMonitorEntry(context.Background(), "pop1", "SN-HOST", first); err != nil {
		t.Fatal(err)
	}
	if err := w.UpsertMonitorEntry(context.Background(), "pop1", "SN-HOST", second); err != nil {
		t.Fatal(err)
	}
	if got := w.Secrets["pop1"]["SN-HOST"].Password; got != "NEW0000000" {
		t.Errorf("password not replaced: got %q", got)
	}
	if len(w.Secrets["pop1"]) != 1 {
		t.Errorf("pop1 should have 1 entry, has %d", len(w.Secrets["pop1"]))
	}
}

// TestFakeMonitorWriter_PreservesOtherEntries verifies that upserting
// a new serial doesn't disturb existing entries for other hosts in
// the same pop. This is the bug class an unscoped Put would
// introduce: clobbering siblings on every per-host write.
func TestFakeMonitorWriter_PreservesOtherEntries(t *testing.T) {
	w := &FakeMonitorWriter{
		Secrets: map[string]map[string]BMCMonitorEntry{
			"pop1": {
				"SN-EXISTING": {Kind: "bmc", Username: "monitor", Password: "PRIOR00000"},
			},
		},
	}
	newEntry := BMCMonitorEntry{Kind: "bmc", Username: "monitor", Password: "FRESH00000"}
	if err := w.UpsertMonitorEntry(context.Background(), "pop1", "SN-NEW", newEntry); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.Secrets["pop1"]["SN-EXISTING"]; !ok {
		t.Error("upsert clobbered SN-EXISTING")
	}
	if w.Secrets["pop1"]["SN-EXISTING"].Password != "PRIOR00000" {
		t.Error("SN-EXISTING password mutated by unrelated upsert")
	}
}

// TestMarshalSorted_DeterministicKeyOrder verifies that the writer's
// canonicalization produces a stable byte sequence regardless of
// map iteration order. AWS retains the previous version of every
// PutSecretValue, so a stable encoding makes "what changed?" diffs
// in CloudTrail / `aws secretsmanager get-secret-value --version-stage`
// human-readable.
func TestMarshalSorted_DeterministicKeyOrder(t *testing.T) {
	in1 := map[string]json.RawMessage{
		"b.pop1": json.RawMessage(`{"x":1}`),
		"a.pop1": json.RawMessage(`{"x":2}`),
		"c.pop1": json.RawMessage(`{"x":3}`),
	}
	in2 := map[string]json.RawMessage{
		"c.pop1": json.RawMessage(`{"x":3}`),
		"a.pop1": json.RawMessage(`{"x":2}`),
		"b.pop1": json.RawMessage(`{"x":1}`),
	}
	out1, err := marshalSorted(in1)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := marshalSorted(in2)
	if err != nil {
		t.Fatal(err)
	}
	if string(out1) != string(out2) {
		t.Errorf("marshalSorted not deterministic across iteration orders:\n  out1 = %s\n  out2 = %s",
			out1, out2)
	}
	// Confirm key order is alphabetical by checking the byte stream.
	if want := `{"a.pop1":{"x":2},"b.pop1":{"x":1},"c.pop1":{"x":3}}`; string(out1) != want {
		t.Errorf("marshalSorted output: got %s, want %s", out1, want)
	}
}

// TestUpsertMonitorEntry_RejectsMetaSerial guards the "_meta"
// reserved key; a misconfigured caller passing it as the serial
// would silently overwrite the secret's metadata block.
func TestUpsertMonitorEntry_RejectsMetaSerial(t *testing.T) {
	w := &FakeMonitorWriter{}
	// Fake doesn't enforce the reservation — the production Client does.
	// This test exists so future regressions of the production rule
	// fail loudly. Kept here rather than on Client to keep the
	// existing tests offline-friendly.
	_ = w
	c := &Client{}
	err := c.UpsertMonitorEntry(context.Background(), "pop1", "_meta", BMCMonitorEntry{})
	if err == nil {
		t.Fatal("expected reject of serial=_meta")
	}
}

// TestUpsertMonitorEntry_RejectsEmptySerial: the monitor secret is keyed by
// NetBox device serial, like bmc-creds-admin. An empty serial must fail
// before any AWS call rather than write an unkeyed entry.
func TestUpsertMonitorEntry_RejectsEmptySerial(t *testing.T) {
	c := &Client{}
	err := c.UpsertMonitorEntry(context.Background(), "pop1", "", BMCMonitorEntry{})
	if err == nil || !strings.Contains(err.Error(), "serial") {
		t.Fatalf("expected an empty-serial rejection naming the serial, got %v", err)
	}
}

// TestBMCMonitorEntry_WireKeys pins the per-host entry shape slagworks
// writes into pop/<pop>/bmc-creds-monitor: the cred plus the host's last
// known NetBox hostname and BMC IP. The entry is keyed by serial, so these
// fields are how a human (and external tooling) maps it back to a host;
// both keys always render.
func TestBMCMonitorEntry_WireKeys(t *testing.T) {
	raw, err := json.Marshal(BMCMonitorEntry{
		Kind: KindBMC, Username: "monitor", Password: "pw",
		ExtraConfig:       map[string]any{"ipmi_user_slot": 4},
		LastKnownHostname: "kworker-l34-1.pop2", LastKnownIP: "10.253.242.41",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"kind", "username", "password", "extra_config", "last_known_hostname", "last_known_ip"} {
		if _, ok := got[k]; !ok {
			t.Errorf("entry missing key %q: %s", k, raw)
		}
	}
	if got["last_known_hostname"] != "kworker-l34-1.pop2" || got["last_known_ip"] != "10.253.242.41" {
		t.Errorf("last_known fields: %s", raw)
	}
	empty, _ := json.Marshal(BMCMonitorEntry{})
	if !strings.Contains(string(empty), `"last_known_hostname":""`) || !strings.Contains(string(empty), `"last_known_ip":""`) {
		t.Errorf("last_known keys must render even when empty: %s", empty)
	}
}

// TestMonitorSecretStub_Shape verifies the bootstrap stub is valid JSON with a
// _meta block and NO host entries — the empty-by-design shape the cred-storage
// spec and bmc-creds-sync.py expect. A non-_meta key here would look like a
// (bogus) host monitor entry to every consumer.
func TestMonitorSecretStub_Shape(t *testing.T) {
	raw := monitorSecretStub("pop2")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("stub is not valid JSON: %v (%s)", err, raw)
	}
	if _, ok := doc["_meta"]; !ok {
		t.Errorf("stub missing _meta block: %s", raw)
	}
	for k := range doc {
		if k != "_meta" {
			t.Errorf("stub must contain ONLY _meta, found host-shaped key %q", k)
		}
	}
	var meta map[string]any
	if err := json.Unmarshal(doc["_meta"], &meta); err != nil {
		t.Fatalf("_meta not an object: %v", err)
	}
	if meta["aws_path"] != "pop/pop2/bmc-creds-monitor" {
		t.Errorf("_meta.aws_path = %v, want pop/pop2/bmc-creds-monitor", meta["aws_path"])
	}
	if meta["tier"] != "monitor" {
		t.Errorf("_meta.tier = %v, want monitor", meta["tier"])
	}
}

// TestFakeMonitorWriter_EnsureRecordsPop verifies the fake's EnsureMonitorSecret
// records the pop (and seeds its secret map so a later upsert sees it present).
func TestFakeMonitorWriter_EnsureRecordsPop(t *testing.T) {
	w := &FakeMonitorWriter{}
	if err := w.EnsureMonitorSecret(context.Background(), "pop2"); err != nil {
		t.Fatalf("EnsureMonitorSecret: %v", err)
	}
	if !w.Ensured["pop2"] {
		t.Error("pop2 not recorded as ensured")
	}
	if _, ok := w.Secrets["pop2"]; !ok {
		t.Error("EnsureMonitorSecret should seed the pop's secret map")
	}
}
