package tui

import "testing"

func TestDecideHold(t *testing.T) {
	cases := []struct {
		name string
		p    HoldPolicy
		want bool
	}{
		// Interactive holds on ANY terminal state (operator quits explicitly);
		// the success/failure flag no longer gates the hold.
		{"interactive success", HoldPolicy{Failed: false, Interactive: true}, true},
		{"interactive failure", HoldPolicy{Failed: true, Interactive: true}, true},
		// Non-interactive auto-exits (don't hang a piped/CI run on a keypress).
		{"non-interactive success", HoldPolicy{Failed: false, Interactive: false}, false},
		{"non-interactive failure", HoldPolicy{Failed: true, Interactive: false}, false},
		// --hold forces a hold even non-interactively (record a CI run).
		{"force-hold non-interactive", HoldPolicy{Interactive: false, ForceHold: true}, true},
		// --no-hold forces teardown even interactively (human wants auto-exit).
		{"force-no-hold interactive", HoldPolicy{Interactive: true, ForceNoHold: true}, false},
		// --no-hold wins over --hold (conservative: a --no-hold caller must not hang).
		{"no-hold beats hold", HoldPolicy{Interactive: true, ForceHold: true, ForceNoHold: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideHold(tc.p); got != tc.want {
				t.Errorf("decideHold(%+v) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}
