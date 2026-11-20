package worker

import (
	"bytes"
	"testing"

	"pgregory.net/rapid"
)

// Property tests for solNoiseFilter.
//
// Oracle: a whole-stream, position-by-position scan written against the
// three noise lines spelled out literally here (not solNoisePatterns, so
// a pattern edited in production is caught as a divergence rather than
// silently agreed with). At each position, if a noise line starts there
// it is skipped whole; otherwise one byte is emitted. That is the
// documented "remove every occurrence, matched anywhere, left to right"
// contract.
//
// Metamorphic relation: the filter's output must not depend on how the
// stream is split into reads.

var oracleNoise = []string{
	"[SOL Session operational.  Use ~? for help]\r\n",
	"Error: No response to keepalive - Terminating session\r\n",
	"Error: No response de-activating SOL payload\r\n",
}

func oracleStrip(in []byte) []byte {
	var out []byte
	for i := 0; i < len(in); {
		matched := false
		for _, p := range oracleNoise {
			if bytes.HasPrefix(in[i:], []byte(p)) {
				i += len(p)
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, in[i])
			i++
		}
	}
	return out
}

// genNoiseStream builds a stream out of tokens chosen to collide with the
// patterns: whole noise lines, proper prefixes of them, near-misses with
// one byte changed, shared prefixes ("Error: No response "), bare CR/LF,
// ANSI escapes and random bytes.
func genNoiseStream(t *rapid.T) []byte {
	n := rapid.IntRange(0, 40).Draw(t, "ntokens")
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		p := oracleNoise[rapid.IntRange(0, len(oracleNoise)-1).Draw(t, "pat")]
		switch rapid.IntRange(0, 7).Draw(t, "tok") {
		case 0, 1:
			b.WriteString(p)
		case 2:
			b.WriteString(p[:rapid.IntRange(0, len(p)-1).Draw(t, "prefixLen")])
		case 3:
			q := []byte(p)
			q[rapid.IntRange(0, len(q)-1).Draw(t, "flip")] ^= 0x20
			b.Write(q)
		case 4:
			b.WriteString(rapid.SampledFrom([]string{"\r\n", "\r", "\n", "Error: No response ", "[SOL ", "\x1b[0m"}).Draw(t, "frag"))
		default:
			b.Write(rapid.SliceOfN(rapid.Byte(), 0, 8).Draw(t, "rand"))
		}
	}
	return b.Bytes()
}

func runChunked(data []byte, cuts []int) []byte {
	var f solNoiseFilter
	var out bytes.Buffer
	prev := 0
	for _, c := range cuts {
		out.Write(f.Filter(data[prev:c]))
		prev = c
	}
	out.Write(f.Filter(data[prev:]))
	out.Write(f.Flush())
	return out.Bytes()
}

func genCuts(t *rapid.T, n int) []int {
	if n == 0 {
		return nil
	}
	k := rapid.IntRange(0, min(n, 30)).Draw(t, "ncuts")
	cuts := make([]int, 0, k)
	prev := 0
	for i := 0; i < k && prev < n; i++ {
		c := rapid.IntRange(prev, n).Draw(t, "cut")
		cuts = append(cuts, c)
		prev = c
	}
	return cuts
}

func TestSOLNoise_Property_MatchesOracleUnderAnyChunking(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		data := genNoiseStream(t)
		want := oracleStrip(data)
		cuts := genCuts(t, len(data))
		got := runChunked(data, cuts)
		if !bytes.Equal(got, want) {
			t.Fatalf("chunked output diverges from oracle\n in   %q\n cuts %v\n got  %q\n want %q", data, cuts, got, want)
		}
		// One-byte-per-read is the extreme split.
		every := make([]int, 0, len(data))
		for i := 1; i < len(data); i++ {
			every = append(every, i)
		}
		if got := runChunked(data, every); !bytes.Equal(got, want) {
			t.Fatalf("byte-at-a-time output diverges\n in   %q\n got  %q\n want %q", data, got, want)
		}
	})
}

// TestSOLNoise_Property_FilterDoesNotAliasInput: the pump hands Filter its
// reusable read buffer and keeps the returned slice in a bytes.Buffer
// only after copying, but carry must never alias the caller's buffer:
// overwriting the input after Filter returns must not change what a
// later call emits.
func TestSOLNoise_Property_FilterDoesNotAliasInput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		data := genNoiseStream(t)
		cuts := genCuts(t, len(data))
		want := oracleStrip(data)

		var f solNoiseFilter
		var out bytes.Buffer
		buf := make([]byte, len(data))
		prev := 0
		emit := func(lo, hi int) {
			chunk := buf[:hi-lo]
			copy(chunk, data[lo:hi])
			res := f.Filter(chunk)
			out.Write(res)
			for i := range chunk {
				chunk[i] = 0xAA // reuse the read buffer, like the pump does
			}
		}
		for _, c := range cuts {
			emit(prev, c)
			prev = c
		}
		emit(prev, len(data))
		out.Write(f.Flush())
		if !bytes.Equal(out.Bytes(), want) {
			t.Fatalf("aliasing changed output\n in   %q\n cuts %v\n got  %q\n want %q", data, cuts, out.Bytes(), want)
		}
	})
}
