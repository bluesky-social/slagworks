package model

import (
	"strings"
	"testing"
)

func TestValidHostname(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	// 63+1+63+1+63+1+61 = 253 characters.
	max253 := label63 + "." + label63 + "." + label63 + "." + strings.Repeat("a", 61)

	for _, s := range []string{
		"web1",
		"oob-gw-3-pop2",
		"kworker-l34-1.pop1",
		"kworker-1.internal.site1.example.net",
		"Web1.Example.NET",
		"1host",
		label63,
		max253,
	} {
		if !ValidHostname(s) {
			t.Errorf("ValidHostname(%q) = false, want true", s)
		}
	}

	for _, s := range []string{
		"",
		"web1\nchain http://203.0.113.9/x.ipxe",
		"web1 break",
		"web1\t",
		"web_1",
		"web1-${net0/mac}",
		`web"1`,
		"wéb1",
		"-web1",
		"web1-",
		"web1.",
		".web1",
		"web1..pop1",
		label63 + "a",
		max253 + "a",
	} {
		if ValidHostname(s) {
			t.Errorf("ValidHostname(%q) = true, want false", s)
		}
	}
}
