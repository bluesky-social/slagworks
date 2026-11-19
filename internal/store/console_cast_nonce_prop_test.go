package store

// Property-based coverage for ScanCastEventsForNonce against real Postgres:
// "did this nonce reach the machine's console stream" must agree with
// bytes.Contains over the concatenated event payloads, whatever bytes the
// nonce holds. The nonce originates in the commission report, which the
// machine under commission writes, so the scan must treat SQL LIKE
// wildcards literally and match nothing for the empty string.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestProp_ScanCastEventsForNonce_MatchesBytesContains(t *testing.T) {
	st := testStore(t)
	_, cast := mustEnsureCast(t, st)
	machineID := cast.MachineID

	alphabet := []byte("ab01%_\\ \n-")
	genBytes := func(label string, min, max int) *rapid.Generator[[]byte] {
		return rapid.Custom(func(t *rapid.T) []byte {
			n := rapid.IntRange(min, max).Draw(t, label+"_n")
			b := make([]byte, n)
			for i := range b {
				b[i] = alphabet[rapid.IntRange(0, len(alphabet)-1).Draw(t, label+"_c")]
			}
			return b
		})
	}

	var tUs int64
	var hits, misses, wild, empty int
	rapid.Check(t, func(t *rapid.T) {
		if err := st.db.Exec("DELETE FROM console_cast_events WHERE cast_id = ?", cast.ID).Error; err != nil {
			t.Fatalf("clear events: %v", err)
		}
		chunks := rapid.SliceOfN(genBytes("chunk", 1, 8), 0, 4).Draw(t, "chunks")
		var full []byte
		var batch CastEventBatch
		batch.CastID = cast.ID
		for _, c := range chunks {
			tUs++
			batch.Events = append(batch.Events, CastEventInput{TUs: tUs, Data: c})
			full = append(full, c...)
		}
		if err := st.AppendConsoleCastEvents(context.Background(), batch); err != nil {
			t.Fatalf("append: %v", err)
		}
		// Bias the nonce toward substrings of the stream so hits happen.
		var nonce []byte
		if len(full) > 0 && rapid.Bool().Draw(t, "from_stream") {
			i := rapid.IntRange(0, len(full)-1).Draw(t, "i")
			j := rapid.IntRange(i, len(full)).Draw(t, "j")
			nonce = append(nonce, full[i:j]...)
		} else {
			nonce = genBytes("nonce", 0, 4).Draw(t, "nonce")
		}

		got, err := st.ScanCastEventsForNonce(context.Background(), machineID, string(nonce), time.Hour)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		// A nonce must be non-empty to identify anything.
		want := len(nonce) > 0 && bytes.Contains(full, nonce)
		if len(nonce) == 0 {
			empty++
		}
		if bytes.ContainsAny(nonce, "%_\\") {
			wild++
		}
		if want {
			hits++
		} else {
			misses++
		}
		if got != want {
			t.Fatalf("ScanCastEventsForNonce(nonce=%q) = %v over stream %q, want %v", nonce, got, full, want)
		}
	})
	t.Logf("coverage: hits=%d misses=%d wildcard_nonces=%d empty_nonces=%d", hits, misses, wild, empty)
}
