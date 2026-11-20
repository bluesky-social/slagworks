package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/proptest/paramsgen"
)

// Property coverage for the operator-facing params redaction.
//
// paramsgen draws a params document with a unique canary at every
// credential position (and inside every value the redaction documents as
// withheld whole) and a unique keep marker everywhere else, then renders it
// with shapes the typed producers never emit. The oracle is paramsgen's
// reference model, written from RedactParams' doc comment over its own AST:
// it shares no code with redactAt.
//
// Properties:
//   - no canary survives, in the raw bytes or in any decoded string;
//   - the output means exactly what the reference model says (so every keep
//     marker, key and non-credential value survives);
//   - the output is valid JSON, redaction is idempotent, the input bytes are
//     not modified;
//   - whatever DecodeParams (the worker's reader) would hand an executor as
//     a credential is not in the redacted view.
//
// Two classes get their own generator options: documents that repeat a key
// (only the last member of a repeated key on a credential path survives, as
// in a jsonb column), and documents that spell a known key in another letter
// case (DecodeParams refuses those on a credential path, so there is nothing
// for the exact-key redaction to miss).

// propStats accumulates generator coverage across cases.
type propStats struct {
	mu sync.Mutex
	m  map[string]int
}

func (s *propStats) add(k string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]int{}
	}
	s.m[k] += n
}

func (s *propStats) report(t *testing.T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%d", k, s.m[k])
	}
	t.Logf("coverage:%s", b.String())
}

func TestProp_SecretPathsMatchReference(t *testing.T) {
	var got [][]string
	for _, p := range SecretParamPaths() {
		got = append(got, []string(p))
	}
	if !reflect.DeepEqual(got, paramsgen.SecretPaths) {
		t.Fatalf("the reference model's secret paths drifted from the secret tags:\n tags %v\n  ref %v\nupdate paramsgen.SecretPaths and its generator", got, paramsgen.SecretPaths)
	}
}

// credentialsOf lists the non-empty credentials a decoded OpParams hands an
// executor.
func credentialsOf(p OpParams) []string {
	var out []string
	add := func(s string) {
		if s != "" {
			out = append(out, s)
		}
	}
	if p.BMCRuntime != nil {
		add(p.BMCRuntime.Password)
	}
	if p.PDURuntime != nil {
		for _, o := range p.PDURuntime.Outlets {
			add(o.Password)
		}
	}
	if p.AMTRuntime != nil {
		add(p.AMTRuntime.Password)
	}
	if p.TailscaleRuntime != nil {
		add(p.TailscaleRuntime.AuthKey)
	}
	if p.MonitorRuntime != nil {
		add(p.MonitorRuntime.Password)
		for _, u := range p.MonitorRuntime.Unconfirmed {
			add(u.Password)
		}
	}
	return out
}

// checkRedaction runs every redaction property on one document. strict adds
// the comparison with the reference model, which only documents whose keys
// are spelled exactly support.
func checkRedaction(t *rapid.T, d *paramsgen.Doc, strict bool, stats *propStats) {
	raw := d.Raw()
	orig := bytes.Clone(raw)
	if !json.Valid(raw) {
		t.Fatalf("generator produced invalid JSON: %s", raw)
	}
	for k, n := range d.Features {
		stats.add(k, n)
	}
	stats.add("docs", 1)
	stats.add("canaries", len(d.Canaries))
	stats.add("keeps", len(d.Keeps))

	out := RedactParams(raw)
	if !bytes.Equal(raw, orig) {
		t.Fatalf("RedactParams modified its input\n was %s\n now %s", orig, raw)
	}
	if !json.Valid(out) {
		t.Fatalf("redacted params are not valid JSON\n  in %s\n out %s", raw, out)
	}
	parsed, err := paramsgen.Parse(out)
	if err != nil {
		t.Fatalf("redacted params do not parse: %v\n out %s", err, out)
	}
	outStrings := paramsgen.Strings(parsed)

	// What the worker's decoder would use as a credential must be gone.
	if p, err := DecodeParams(raw); err == nil {
		stats.add("decodable", 1)
		for _, c := range credentialsOf(p) {
			stats.add("decoded_credentials", 1)
			if c == RedactedMarker {
				continue
			}
			for _, s := range outStrings {
				if strings.Contains(s, c) {
					t.Fatalf("a credential DecodeParams accepts survives redaction: %q\n  in %s\n out %s", c, raw, out)
				}
			}
			if bytes.Contains(out, []byte(c)) {
				t.Fatalf("a credential DecodeParams accepts survives redaction in the raw bytes: %q\n  in %s\n out %s", c, raw, out)
			}
		}
	} else {
		stats.add("undecodable", 1)
	}
	if p, err := DecodeParams(out); err == nil {
		for _, c := range credentialsOf(p) {
			if c != RedactedMarker {
				t.Fatalf("the redacted view still decodes to a credential %q\n  in %s\n out %s", c, raw, out)
			}
		}
	}

	if !strict {
		return
	}
	for _, c := range d.Canaries {
		if bytes.Contains(out, []byte(c)) {
			t.Fatalf("canary %s survives redaction in the raw bytes\n  in %s\n out %s", c, raw, out)
		}
		for _, s := range outStrings {
			if strings.Contains(s, c) {
				t.Fatalf("canary %s survives redaction\n  in %s\n out %s", c, raw, out)
			}
		}
	}
	want := paramsgen.Canon(paramsgen.RefRedact(d.Root))
	for _, c := range d.Canaries {
		if strings.Contains(want, c) {
			t.Fatalf("harness bug: the reference model leaves canary %s in place: %s", c, want)
		}
	}
	for _, k := range d.Keeps {
		// A keep marker under a duplicated key or a withheld shape may be
		// legitimately gone; the reference decides.
		if strings.Contains(want, k) {
			stats.add("keeps_expected_to_survive", 1)
		}
	}
	if got := paramsgen.Canon(parsed); got != want {
		t.Fatalf("redacted params differ from the reference model\n   in %s\n  out %s\n  got %s\n want %s", raw, out, got, want)
	}
	// Idempotent: a second pass means the same thing and is a fixpoint.
	again := RedactParams(out)
	parsedAgain, err := paramsgen.Parse(again)
	if err != nil {
		t.Fatalf("second redaction does not parse: %v", err)
	}
	if paramsgen.Canon(parsedAgain) != want {
		t.Fatalf("redaction is not idempotent\n first %s\nsecond %s", out, again)
	}
	if third := RedactParams(again); !bytes.Equal(third, again) {
		t.Fatalf("redaction does not reach a fixpoint\nsecond %s\n third %s", again, third)
	}
}

func TestProp_RedactParams_MatchesReference(t *testing.T) {
	var stats propStats
	defer stats.report(t)
	rapid.Check(t, func(t *rapid.T) {
		d := paramsgen.Draw(t, paramsgen.Options{Malformed: true, NonObjectRoot: true})
		checkRedaction(t, d, true, &stats)
	})
}

// TestProp_RedactParams_TypedProducerShapes restricts the generator to what
// json.Marshal(OpParams) can emit plus unknown keys, the class every
// production op is in.
func TestProp_RedactParams_TypedProducerShapes(t *testing.T) {
	var stats propStats
	defer stats.report(t)
	rapid.Check(t, func(t *rapid.T) {
		d := paramsgen.Draw(t, paramsgen.Options{TypedValues: true})
		checkRedaction(t, d, true, &stats)
	})
}

// TestProp_RedactParams_DuplicateKeys: a repeated key is valid JSON, and
// DecodeParams merges repeated object members field by field, so a credential
// in an earlier member still reaches the worker. No member may show it.
func TestProp_RedactParams_DuplicateKeys(t *testing.T) {
	var stats propStats
	defer stats.report(t)
	rapid.Check(t, func(t *rapid.T) {
		d := paramsgen.Draw(t, paramsgen.Options{DupKeys: true, Malformed: true})
		checkRedaction(t, d, true, &stats)
	})
}

// caseVariantOnPath reports whether n, walked along a credential path, has a
// key that matches the path's key in every way but letter case: a key
// encoding/json would read as the field and the exact-key redaction would
// not see.
func caseVariantOnPath(n *paramsgen.Node, path []string) bool {
	if len(path) == 0 {
		return false
	}
	if path[0] == paramsgen.Each {
		for _, e := range n.Elems {
			if caseVariantOnPath(e, path[1:]) {
				return true
			}
		}
		return false
	}
	for _, m := range n.Members {
		if m.Key != path[0] && strings.EqualFold(m.Key, path[0]) {
			return true
		}
		if m.Key == path[0] && caseVariantOnPath(m.Val, path[1:]) {
			return true
		}
	}
	return false
}

// TestProp_RedactParams_CoversWhatDecodeAccepts: encoding/json matches keys
// to struct fields case-insensitively, the redaction matches them exactly.
// DecodeParams must refuse a document that spells a key on a credential path
// in another letter case, and whatever it does accept as a credential is
// absent from the redacted view.
func TestProp_RedactParams_CoversWhatDecodeAccepts(t *testing.T) {
	var stats propStats
	defer stats.report(t)
	rapid.Check(t, func(t *rapid.T) {
		d := paramsgen.Draw(t, paramsgen.Options{CaseKeys: true, TypedValues: true})
		for _, p := range paramsgen.SecretPaths {
			if !caseVariantOnPath(d.Root, p) {
				continue
			}
			stats.add("case_variant_on_secret_path", 1)
			if _, err := DecodeParams(d.Raw()); err == nil {
				t.Fatalf("DecodeParams accepted a key on credential path %v spelled in another letter case: %s", p, d.Raw())
			}
			break
		}
		checkRedaction(t, d, false, &stats)
	})
}

// TestProp_RedactParams_RoundTripsTypedParams generates the typed union
// directly (the production producer), marshals it, redacts, and decodes:
// every credential is the marker or empty, and every other field is
// unchanged.
func TestProp_RedactParams_RoundTripsTypedParams(t *testing.T) {
	str := func(t *rapid.T, label string) string {
		return rapid.SampledFrom([]string{"", "a", `q"uo\te`, "<&>", "é☃", "[redacted]", " sp ace "}).Draw(t, label)
	}
	rapid.Check(t, func(t *rapid.T) {
		n := 0
		secret := func(label string) string {
			if rapid.IntRange(0, 3).Draw(t, label) == 0 {
				return ""
			}
			n++
			return fmt.Sprintf("%sCNR%dQZ%s", str(t, label+"pre"), n, str(t, label+"post"))
		}
		var p OpParams
		p.Destructive = rapid.Bool().Draw(t, "destructive")
		p.BootDevice = str(t, "boot_device")
		if rapid.Bool().Draw(t, "bmc") {
			p.BMCRuntime = &BMCRuntime{ExpectedChassisSerial: str(t, "serial"), Username: str(t, "user"),
				Password: secret("bmcpw"), ExtraConfig: map[string]any{"password": str(t, "extra")}}
		}
		if rapid.Bool().Draw(t, "pdu") {
			p.PDURuntime = &PDURuntime{DownstreamSerial: str(t, "ds"), Outlets: []PDUOutlet{}}
			for i := range rapid.IntRange(0, 4).Draw(t, "outlets") {
				p.PDURuntime.Outlets = append(p.PDURuntime.Outlets, PDUOutlet{
					PDUHost: str(t, "host"), Outlet: i, Username: str(t, "ouser"), Password: secret("pdupw")})
			}
		}
		if rapid.Bool().Draw(t, "amt") {
			p.AMTRuntime = &AMTRuntime{Host: str(t, "ahost"), Username: str(t, "auser"), Password: secret("amtpw")}
		}
		if rapid.Bool().Draw(t, "ts") {
			p.TailscaleRuntime = &TailscaleRuntime{AuthKey: secret("tskey"), Hostname: str(t, "tshost")}
		}
		if rapid.Bool().Draw(t, "mon") {
			p.MonitorRuntime = &MonitorRuntime{Password: secret("monpw")}
			for range rapid.IntRange(0, 3).Draw(t, "unconfirmed") {
				p.MonitorRuntime.Unconfirmed = append(p.MonitorRuntime.Unconfirmed, UnconfirmedPassword{Password: secret("unconfpw")})
			}
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		out := RedactParams(raw)
		for i := 1; i <= n; i++ {
			if c := fmt.Sprintf("CNR%dQZ", i); bytes.Contains(out, []byte(c)) {
				t.Fatalf("credential %s survives redaction\n  in %s\n out %s", c, raw, out)
			}
		}
		got, err := DecodeParams(out)
		if err != nil {
			t.Fatalf("redacted params no longer decode: %v\n out %s", err, out)
		}
		// Independent expectation: the input with each non-empty
		// credential replaced by the marker.
		want, err := DecodeParams(raw)
		if err != nil {
			t.Fatal(err)
		}
		mark := func(s *string) {
			if *s != "" {
				*s = "[redacted]"
			}
		}
		if want.BMCRuntime != nil {
			mark(&want.BMCRuntime.Password)
		}
		if want.PDURuntime != nil {
			for i := range want.PDURuntime.Outlets {
				mark(&want.PDURuntime.Outlets[i].Password)
			}
		}
		if want.AMTRuntime != nil {
			mark(&want.AMTRuntime.Password)
		}
		if want.TailscaleRuntime != nil {
			mark(&want.TailscaleRuntime.AuthKey)
		}
		if want.MonitorRuntime != nil {
			mark(&want.MonitorRuntime.Password)
			for i := range want.MonitorRuntime.Unconfirmed {
				mark(&want.MonitorRuntime.Unconfirmed[i].Password)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("redaction changed a non-credential field\n  in %s\n out %s\n got %+v\nwant %+v", raw, out, got, want)
		}
	})
}
