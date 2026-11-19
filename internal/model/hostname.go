package model

import "strings"

// ValidHostname reports whether s is a valid hostname (RFC 1123):
// dot-separated labels of ASCII letters, digits and hyphens, each 1-63
// characters and neither starting nor ending with a hyphen, at most 253
// characters in all, with no trailing dot.
//
// NetBox stores a device name as free text, and slagworks derives hostnames
// from it: the deployed host's /etc/hostname and /etc/hosts entry, and its
// tailnet name, which rides the deploy iPXE script's kernel line. A value
// that passes cannot carry whitespace, a newline, or a character iPXE or
// the kernel cmdline would interpret, so it is safe in all of those places.
func ValidHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if !validHostnameLabel(label) {
			return false
		}
	}
	return true
}

func validHostnameLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for _, c := range []byte(l) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}
