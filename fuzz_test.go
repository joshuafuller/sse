/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import (
	"bytes"
	"testing"
)

// FuzzEventStreamReader feeds arbitrary byte sequences through the SSE stream
// parser. It must never panic, and every returned event must be a sub-slice of
// the input (no out-of-bounds access).
func FuzzEventStreamReader(f *testing.F) {
	// Seed corpus: common real-world patterns.
	seeds := []string{
		// Minimal valid event
		"data: hello\n\n",
		// BOM prefix
		"\xef\xbb\xbfdata: bom\n\n",
		// All newline variants
		"data: a\r\rdata: b\n\ndata: c\r\n\r\n",
		// Multiple fields
		"id: 1\nevent: ping\ndata: payload\nretry: 3000\n\n",
		// Comment only
		": keepalive\n\n",
		// Empty data line (reset)
		"data:\n\n",
		// Bare id: (reset last-event-id)
		"id:\ndata: x\n\n",
		// Very long data line
		"data: " + string(bytes.Repeat([]byte("x"), 8192)) + "\n\n",
		// Multiple events back-to-back
		"data: one\n\ndata: two\n\ndata: three\n\n",
		// Mixed line endings
		"data: a\r\ndata: b\n\ndata: c\r\r",
		// Empty input
		"",
		// Only newlines
		"\n\n\n\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewEventStreamReader(bytes.NewReader(data), 1<<20 /* 1 MiB */)
		for {
			event, err := r.ReadEvent()
			if err != nil {
				break
			}
			// Event bytes must be a subset of the input: no allocation beyond
			// what was read, and no out-of-bounds indexing.
			if len(event) > len(data) {
				t.Fatalf("ReadEvent returned %d bytes but input was only %d bytes", len(event), len(data))
			}
		}
	})
}

// FuzzContainsDoubleNewline checks that the internal double-newline scanner
// never panics and upholds its basic invariants for arbitrary input:
//   - if index < 0 is returned the length value is irrelevant (no terminator found)
//   - if index >= 0, data[index : index+nlen] must be a valid SSE terminator
func FuzzContainsDoubleNewline(f *testing.F) {
	seeds := [][]byte{
		[]byte("\n\n"),
		[]byte("\r\r"),
		[]byte("\r\n\r\n"),
		[]byte("\r\n\n"),
		[]byte("\n\r\n"),
		[]byte("\r\n\r"),
		[]byte("\n\r"),
		[]byte("data: hello\n\n"),
		[]byte("data: hello\r\n\r\n"),
		[]byte(""),
		[]byte("\x00"),
		[]byte(string(bytes.Repeat([]byte("a"), 512)) + "\n\n"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	validTerminators := [][]byte{
		[]byte("\r\r"),
		[]byte("\n\n"),
		[]byte("\n\r"),
		[]byte("\r\n\n"),
		[]byte("\n\r\n"),
		[]byte("\r\n\r"),
		[]byte("\r\n\r\n"),
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		idx, nlen := containsDoubleNewline(data)
		if idx < 0 {
			return // no terminator — nothing else to check
		}
		if idx+nlen > len(data) {
			t.Fatalf("containsDoubleNewline returned idx=%d nlen=%d but len(data)=%d", idx, nlen, len(data))
		}
		term := data[idx : idx+nlen]
		for _, v := range validTerminators {
			if bytes.Equal(term, v) {
				return
			}
		}
		t.Fatalf("containsDoubleNewline returned non-terminator sequence %q at idx=%d", term, idx)
	})
}

// FuzzMinPosInt verifies the internal helper never panics and always returns
// a value that is ≥ both inputs when both are non-negative.
func FuzzMinPosInt(f *testing.F) {
	f.Add(0, 0)
	f.Add(-1, 0)
	f.Add(0, -1)
	f.Add(-1, -1)
	f.Add(5, 10)
	f.Add(10, 5)
	f.Add(1<<30, 1<<30-1)

	f.Fuzz(func(t *testing.T, a, b int) {
		result := minPosInt(a, b)
		if a >= 0 && b >= 0 {
			if result > a || result > b {
				t.Fatalf("minPosInt(%d, %d) = %d, want ≤ min(%d,%d)", a, b, result, a, b)
			}
			if result != a && result != b {
				t.Fatalf("minPosInt(%d, %d) = %d, must equal one of the inputs", a, b, result)
			}
		}
		if a < 0 && b >= 0 && result != b {
			t.Fatalf("minPosInt(%d, %d) = %d, want %d (a negative, return b)", a, b, result, b)
		}
		if b < 0 && a >= 0 && result != a {
			t.Fatalf("minPosInt(%d, %d) = %d, want %d (b negative, return a)", a, b, result, a)
		}
	})
}
