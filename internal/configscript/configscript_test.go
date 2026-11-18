package configscript

import (
	"strings"
	"testing"
)

var testFields = map[string]struct{}{
	"system_manufacturer": {},
	"system_product":      {},
}

func mustParse(t *testing.T, content string) *Script {
	t.Helper()
	s, err := Parse("test-script", []byte(content), testFields)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

// resolverFor builds a resolve closure over a fixed manufacturer/product so
// the engine tests can match without a commission-data dependency.
func resolverFor(manufacturer, product string) func(string) string {
	return func(field string) string {
		switch field {
		case "system_manufacturer":
			return manufacturer
		case "system_product":
			return product
		default:
			return ""
		}
	}
}

// --- Parse ---

func TestParse_NoRules(t *testing.T) {
	s := mustParse(t, "#!/usr/bin/env bash\necho hello\n")
	if len(s.Rules) != 0 {
		t.Errorf("expected 0 rules, got %d", len(s.Rules))
	}
}

func TestParse_ExactMatch(t *testing.T) {
	s := mustParse(t, `#!/usr/bin/env bash
# slagworks:match system_product=RS501A-E12-RS4U
echo hello
`)
	if len(s.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(s.Rules))
	}
	if s.Rules[0].Field != "system_product" {
		t.Errorf("field: got %q, want system_product", s.Rules[0].Field)
	}
	if s.Rules[0].Value != "RS501A-E12-RS4U" {
		t.Errorf("value: got %q, want RS501A-E12-RS4U", s.Rules[0].Value)
	}
	if s.Rules[0].pattern != nil {
		t.Error("exact match rule should not have compiled pattern")
	}
}

func TestParse_RegexMatch(t *testing.T) {
	s := mustParse(t, `#!/usr/bin/env bash
# slagworks:match system_manufacturer=/ASUSTeK.*/
echo hello
`)
	if len(s.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(s.Rules))
	}
	if s.Rules[0].pattern == nil {
		t.Fatal("regex rule should have compiled pattern")
	}
	if !s.Rules[0].pattern.MatchString("ASUSTeK Computer INC.") {
		t.Error("pattern should match ASUSTeK Computer INC.")
	}
	if s.Rules[0].pattern.MatchString("Intel Corporation") {
		t.Error("pattern should not match Intel Corporation")
	}
}

func TestParse_Priority(t *testing.T) {
	s := mustParse(t, `#!/usr/bin/env bash
# slagworks:priority 100
echo hello
`)
	if s.Priority != 100 {
		t.Errorf("expected priority 100, got %d", s.Priority)
	}
}

func TestParse_MultipleRules(t *testing.T) {
	s := mustParse(t, `#!/usr/bin/env bash
# slagworks:match system_product=RS501A-E12-RS4U
# slagworks:match system_manufacturer=/ASUSTeK.*/
# slagworks:priority 50
echo hello
`)
	if len(s.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(s.Rules))
	}
	if s.Priority != 50 {
		t.Errorf("expected priority 50, got %d", s.Priority)
	}
}

func TestParse_UnknownFieldRejected(t *testing.T) {
	_, err := Parse("bad", []byte("#!/usr/bin/env bash\n# slagworks:match bogus_field=x\n"), testFields)
	if err == nil {
		t.Fatal("expected error for unknown match field")
	}
	if !strings.Contains(err.Error(), "unknown match field") {
		t.Errorf("error should mention unknown match field, got: %v", err)
	}
}

func TestParse_StopsAtNonComment(t *testing.T) {
	// slagworks:match after code should be ignored
	s := mustParse(t, `#!/usr/bin/env bash
set -euo pipefail
# slagworks:match system_product=IGNORED
echo hello
`)
	if len(s.Rules) != 0 {
		t.Errorf("rules after code should be ignored, got %d", len(s.Rules))
	}
}

func TestParse_InvalidRegex(t *testing.T) {
	_, err := Parse("bad", []byte("#!/usr/bin/env bash\n# slagworks:match system_product=/[invalid/\n"), testFields)
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestParse_InvalidPriority(t *testing.T) {
	_, err := Parse("bad", []byte("#!/usr/bin/env bash\n# slagworks:priority notanumber\n"), testFields)
	if err == nil {
		t.Fatal("expected error for non-integer priority")
	}
}

// --- Match ---

func TestMatch_NoScripts(t *testing.T) {
	got, err := Match(nil, resolverFor("ASUS", "RS501A"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got != nil {
		t.Error("expected nil with no scripts")
	}
}

func TestMatch_NoRulesNeverMatches(t *testing.T) {
	s := mustParse(t, "#!/usr/bin/env bash\n# slagworks:priority 100\necho hello\n")
	got, err := Match([]Script{*s}, resolverFor("ASUS", "RS501A"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got != nil {
		t.Error("script with no rules should never match")
	}
}

func TestMatch_ExactHit(t *testing.T) {
	s := mustParse(t, "#!/usr/bin/env bash\n# slagworks:match system_product=RS501A-E12-RS4U\n# slagworks:priority 100\n")
	got, err := Match([]Script{*s}, resolverFor("ASUSTeK", "RS501A-E12-RS4U"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got == nil {
		t.Fatal("expected match, got nil")
	}
}

func TestMatch_ExactMiss(t *testing.T) {
	s := mustParse(t, "#!/usr/bin/env bash\n# slagworks:match system_product=RS501A-E12-RS4U\n# slagworks:priority 100\n")
	got, err := Match([]Script{*s}, resolverFor("ASUSTeK", "DIFFERENT-MODEL"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got != nil {
		t.Error("expected no match for different product")
	}
}

func TestMatch_RegexHit(t *testing.T) {
	s := mustParse(t, "#!/usr/bin/env bash\n# slagworks:match system_manufacturer=/ASUSTeK.*/\n# slagworks:priority 10\n")
	got, err := Match([]Script{*s}, resolverFor("ASUSTeK Computer INC.", "RS501A"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got == nil {
		t.Fatal("expected regex match")
	}
}

func TestMatch_RegexMiss(t *testing.T) {
	s := mustParse(t, "#!/usr/bin/env bash\n# slagworks:match system_manufacturer=/ASUSTeK.*/\n# slagworks:priority 10\n")
	got, err := Match([]Script{*s}, resolverFor("Intel Corporation", "RS501A"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got != nil {
		t.Error("expected no match for different manufacturer")
	}
}

func TestMatch_HigherPriorityWins(t *testing.T) {
	low := mustParse(t, "#!/usr/bin/env bash\n# slagworks:match system_product=RS501A\n# slagworks:priority 10\n")
	low.Content = []byte("low")
	high := mustParse(t, "#!/usr/bin/env bash\n# slagworks:match system_product=RS501A\n# slagworks:priority 100\n")
	high.Content = []byte("high")

	got, err := Match([]Script{*low, *high}, resolverFor("ASUS", "RS501A"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got == nil {
		t.Fatal("expected a match")
	}
	if string(got.Content) != "high" {
		t.Errorf("expected high-priority script, got content %q", string(got.Content))
	}
}

func TestMatch_AmbiguousSamePriorityErrors(t *testing.T) {
	// Two scripts that both match the host at the SAME priority is ambiguous:
	// priority is the tiebreaker, so a tie has no deterministic winner. Match
	// must error (so preflight rejects the op) rather than coin-flip. This is
	// the exact condition that arose with a chassis-family regex script and an
	// exact-match script both at priority 200.
	exact := mustParse(t, "#!/usr/bin/env bash\n# slagworks:match system_product=AS -1115HS-TNR\n# slagworks:priority 200\n")
	exact.Path = "exact-1115hs"
	family := mustParse(t, "#!/usr/bin/env bash\n# slagworks:match system_product=/^AS -(1115HS|1015CS)-TNR$/\n# slagworks:priority 200\n")
	family.Path = "tnr-family"

	got, err := Match([]Script{*exact, *family}, resolverFor("Supermicro", "AS -1115HS-TNR"))
	if err == nil {
		t.Fatal("expected ambiguity error when two scripts tie at the same priority")
	}
	if got != nil {
		t.Errorf("expected nil script on ambiguity, got %v", got)
	}
	// Error names both contenders + the priority so the operator can fix it.
	for _, want := range []string{"exact-1115hs", "tnr-family", "200", "ambiguous"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error missing %q: %v", want, err)
		}
	}
}

func TestMatch_DifferentPriorityIsNotAmbiguous(t *testing.T) {
	// Same host matched by two scripts at DIFFERENT priorities is NOT
	// ambiguous — the higher one deterministically wins, no error. Guards
	// against the ambiguity check over-firing on the normal precedence case.
	low := mustParse(t, "#!/usr/bin/env bash\n# slagworks:match system_product=RS501A\n# slagworks:priority 10\n")
	low.Content = []byte("low")
	high := mustParse(t, "#!/usr/bin/env bash\n# slagworks:match system_product=RS501A\n# slagworks:priority 100\n")
	high.Content = []byte("high")

	got, err := Match([]Script{*low, *high}, resolverFor("ASUS", "RS501A"))
	if err != nil {
		t.Fatalf("two matches at different priorities must not error: %v", err)
	}
	if got == nil || string(got.Content) != "high" {
		t.Fatalf("expected high-priority winner, got %v", got)
	}
}

func TestMatch_AllRulesMustMatch(t *testing.T) {
	// product matches, manufacturer doesn't → no match
	s := mustParse(t, `#!/usr/bin/env bash
# slagworks:match system_product=RS501A
# slagworks:match system_manufacturer=ASUS
# slagworks:priority 10
`)
	got, err := Match([]Script{*s}, resolverFor("Intel", "RS501A"))
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got != nil {
		t.Error("expected no match when second rule fails")
	}
}
