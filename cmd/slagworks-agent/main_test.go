//go:build !integration

package main

import (
	"testing"
)

func TestCmdlineHas(t *testing.T) {
	cases := []struct {
		cmdline string
		want    string
		expect  bool
	}{
		{"BOOT_IMAGE=vmlinuz slagworks.no_reboot ro", "slagworks.no_reboot", true},
		{"BOOT_IMAGE=vmlinuz quiet ro", "slagworks.no_reboot", false},
		{"slagworks.machine=abc-123 slagworks.op=def-456 ro", "slagworks.machine", true},
		{"slagworks.machine_id=abc", "slagworks.machine", false}, // exact key, not prefix
		{"", "anything", false},
		{"  ", "x", false},
	}
	for _, c := range cases {
		t.Run(c.cmdline+"|"+c.want, func(t *testing.T) {
			got := cmdlineHas(c.cmdline, c.want)
			if got != c.expect {
				t.Errorf("cmdlineHas(%q, %q) = %v, want %v",
					c.cmdline, c.want, got, c.expect)
			}
		})
	}
}

func TestCompareUUIDs(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		actual   string
		wantErr  bool
	}{
		{"exact match", "550e8400-e29b-41d4-a716-446655440000", "550e8400-e29b-41d4-a716-446655440000", false},
		{"case insensitive", "550E8400-E29B-41D4-A716-446655440000", "550e8400-e29b-41d4-a716-446655440000", false},
		{"mismatch", "550e8400-e29b-41d4-a716-446655440000", "660e8400-e29b-41d4-a716-446655440000", true},
		{"invalid expected", "not-a-uuid", "550e8400-e29b-41d4-a716-446655440000", true},
		{"invalid actual", "550e8400-e29b-41d4-a716-446655440000", "not-a-uuid", true},
		{"empty expected", "", "550e8400-e29b-41d4-a716-446655440000", true},
		{"empty actual", "550e8400-e29b-41d4-a716-446655440000", "", true},
		{"not settable", "550e8400-e29b-41d4-a716-446655440000", "Not Settable", true},
		{"not present", "550e8400-e29b-41d4-a716-446655440000", "Not Present", true},
		{"whitespace only expected", "  ", "550e8400-e29b-41d4-a716-446655440000", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := compareUUIDs(tt.expected, tt.actual)
			if (err != nil) != tt.wantErr {
				t.Errorf("compareUUIDs(%q, %q) error = %v, wantErr %v", tt.expected, tt.actual, err, tt.wantErr)
			}
		})
	}
}
