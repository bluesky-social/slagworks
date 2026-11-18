//go:build integration

package qemutest

import "testing"

func TestParseQEMUVersion(t *testing.T) {
	cases := map[string][2]int{
		"QEMU emulator version 8.2.2 (Debian 1:8.2.2+ds-0ubuntu1.10)\nCopyright (c) 2003-2023": {8, 2},
		"QEMU emulator version 10.1.0 (Debian 1:10.1.0+ds-5ubuntu2)":                           {10, 1},
		"QEMU emulator version 9.2.1":                                                          {9, 2},
	}
	for out, want := range cases {
		major, minor, err := parseQEMUVersion(out)
		if err != nil || major != want[0] || minor != want[1] {
			t.Errorf("parseQEMUVersion(%q) = %d.%d, %v; want %d.%d", out, major, minor, err, want[0], want[1])
		}
	}
	if _, _, err := parseQEMUVersion("not qemu"); err == nil {
		t.Error("want an error for output with no version")
	}
}

func TestReconnectOption(t *testing.T) {
	cases := []struct {
		major, minor, seconds int
		want                  string
	}{
		{8, 2, 2, "reconnect=2"},
		{9, 1, 10, "reconnect=10"},
		{9, 2, 2, "reconnect-ms=2000"},
		{10, 0, 10, "reconnect-ms=10000"},
	}
	for _, c := range cases {
		if got := reconnectOption(c.major, c.minor, c.seconds); got != c.want {
			t.Errorf("reconnectOption(%d, %d, %d) = %q, want %q", c.major, c.minor, c.seconds, got, c.want)
		}
	}
}
