//go:build integration

package qemutest

import "testing"

func TestFirstCPUInList(t *testing.T) {
	cases := map[string]string{
		"0-3":         "0",
		"\t160-223\n": "160",
		"5":           "5",
		"2,4-7":       "2",
	}
	for list, want := range cases {
		got, err := firstCPUInList(list)
		if err != nil || got != want {
			t.Errorf("firstCPUInList(%q) = %q, %v; want %q", list, got, err, want)
		}
	}
	for _, list := range []string{"", "-3", "cpu0"} {
		if got, err := firstCPUInList(list); err == nil {
			t.Errorf("firstCPUInList(%q) = %q, want an error", list, got)
		}
	}
}
