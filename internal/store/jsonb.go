package store

import (
	"bytes"
	"strconv"
)

// jsonbSafe returns raw, a valid JSON document, with every string escape a
// Postgres jsonb column refuses replaced by \ufffd (U+FFFD, the Unicode
// replacement character). jsonb refuses two escapes that JSON allows:
// \u0000, because a Postgres string cannot hold a NUL (SQLSTATE 22P05), and
// a UTF-16 surrogate escape that is not part of a high-low pair (22P02).
//
// Op results and commission inventory carry text from outside slagworks
// verbatim: a config script's stderr, a BMC's or PDU's error body, tool
// output, lldpctl's JSON. Writing such a document unchanged makes the whole
// transaction fail, and none of the writers' callers can recover from that:
// the worker resends an op_update or a commission report the write
// refused until its op times out. Every write of operations.result and
// machines.commission_data therefore goes through jsonbSafe.
//
// raw is returned unchanged, without a copy, when it holds no such escape.
// Escapes are read left to right and each is consumed whole, so in \\u0000
// (an escaped backslash, then the text u0000) the u is never taken for the
// start of an escape.
func jsonbSafe(raw []byte) []byte {
	if !bytes.Contains(raw, []byte(`\u`)) {
		return raw
	}
	var out []byte // allocated at the first replacement
	copied := 0    // raw[:copied] is already in out
	for i := 0; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		if raw[i+1] != 'u' {
			i++ // a two-byte escape such as \\ or \"
			continue
		}
		unit, ok := utf16Escape(raw, i)
		if !ok {
			i++ // not valid JSON; leave it for Postgres to reject
			continue
		}
		switch {
		case isHighSurrogate(unit):
			if low, ok := utf16Escape(raw, i+6); ok && isLowSurrogate(low) {
				i += 11 // a surrogate pair is one character; keep it
				continue
			}
		case unit != 0 && !isLowSurrogate(unit):
			i += 5
			continue
		}
		out = append(out, raw[copied:i]...)
		out = append(out, `\ufffd`...)
		i += 5
		copied = i + 1
	}
	if out == nil {
		return raw
	}
	return append(out, raw[copied:]...)
}

// utf16Escape decodes the \uXXXX escape at raw[i:], reporting false when
// raw[i:] does not start with one.
func utf16Escape(raw []byte, i int) (uint16, bool) {
	if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
		return 0, false
	}
	v, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

func isHighSurrogate(u uint16) bool { return u >= 0xd800 && u <= 0xdbff }

func isLowSurrogate(u uint16) bool { return u >= 0xdc00 && u <= 0xdfff }
