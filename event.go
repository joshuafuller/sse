/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"time"
)

// Event holds all of the event source fields.
type Event struct {
	timestamp time.Time
	IDPresent bool   // true if an id: field was seen in this event
	ID        []byte
	Data      []byte
	Event     []byte
	Retry     []byte
	Comment   []byte
}

func (e *Event) hasContent() bool {
	return len(e.ID) > 0 || len(e.Data) > 0 || len(e.Event) > 0 || len(e.Retry) > 0
}

// EventStreamReader scans an io.Reader looking for EventStream messages.
type EventStreamReader struct {
	scanner *bufio.Scanner
}

// stripBOM removes a leading UTF-8 BOM (EF BB BF) if present.
func stripBOM(r io.Reader) io.Reader {
	buf := make([]byte, 3)
	n, _ := io.ReadFull(r, buf)
	if n == 3 && buf[0] == 0xEF && buf[1] == 0xBB && buf[2] == 0xBF {
		return r // BOM consumed
	}
	return io.MultiReader(bytes.NewReader(buf[:n]), r)
}

// NewEventStreamReader creates an instance of EventStreamReader.
func NewEventStreamReader(eventStream io.Reader, maxBufferSize int) *EventStreamReader {
	scanner := bufio.NewScanner(stripBOM(eventStream))
	initBufferSize := minPosInt(4096, maxBufferSize)
	scanner.Buffer(make([]byte, initBufferSize), maxBufferSize)

	split := func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}

		// We have a full event payload to parse.
		if i, nlen := containsDoubleNewline(data); i >= 0 {
			return i + nlen, data[0:i], nil
		}
		// If we're at EOF, we have all of the data.
		if atEOF {
			return len(data), data, nil
		}
		// Request more data.
		return 0, nil, nil
	}
	// Set the split function for the scanning operation.
	scanner.Split(split)

	return &EventStreamReader{
		scanner: scanner,
	}
}

// Returns a tuple containing the index of a double newline, and the number of bytes
// represented by that sequence. If no double newline is present, the first value
// will be negative.
//
// Per WHATWG SSE §9.2.6, a line ending is \r, \n, or \r\n (treated as one).
// A double newline (empty line = event dispatch) therefore covers all pairings:
//
//	\r\r        (2) — bare CR + bare CR
//	\n\n        (2) — LF + LF
//	\n\r        (2) — LF + bare CR      [sse-frd: previously missing]
//	\r\n\n      (3) — CRLF + LF
//	\n\r\n      (3) — LF + CRLF
//	\r\n\r      (3) — CRLF + bare CR    [sse-frd: previously missing]
//	\r\n\r\n    (4) — CRLF + CRLF
func containsDoubleNewline(data []byte) (int, int) {
	// Search for each potentially valid sequence of newline characters.
	// Per WHATWG SSE §9.2.6, all pairings of the three line endings are valid
	// event terminators. Longer patterns are searched first so that their
	// positions can take precedence over shorter sub-pattern matches.
	crcr := bytes.Index(data, []byte("\r\r"))
	lflf := bytes.Index(data, []byte("\n\n"))
	lfcr := bytes.Index(data, []byte("\n\r"))
	crlflf := bytes.Index(data, []byte("\r\n\n"))
	lfcrlf := bytes.Index(data, []byte("\n\r\n"))
	crlfcr := bytes.Index(data, []byte("\r\n\r"))
	crlfcrlf := bytes.Index(data, []byte("\r\n\r\n"))

	// Find the earliest position of a double newline combination.
	minPos := minPosInt(crcr, minPosInt(lflf, minPosInt(lfcr, minPosInt(crlflf, minPosInt(lfcrlf, minPosInt(crlfcr, crlfcrlf))))))
	if minPos < 0 {
		return minPos, 2
	}

	// Determine the length of the terminator sequence.
	// When a longer pattern and a shorter sub-pattern share the same start
	// position, the longer pattern wins (e.g. \r\n\r\n beats \r\n\r).
	nlen := 2
	switch minPos {
	case crlfcrlf:
		nlen = 4
	case crlflf, lfcrlf, crlfcr:
		nlen = 3
	}
	return minPos, nlen
}

// Returns the minimum non-negative value out of the two values. If both
// are negative, a negative value is returned.
func minPosInt(a, b int) int {
	if a < 0 {
		return b
	}
	if b < 0 {
		return a
	}
	if a > b {
		return b
	}
	return a
}

// ReadEvent scans the EventStream for events.
func (e *EventStreamReader) ReadEvent() ([]byte, error) {
	if e.scanner.Scan() {
		event := e.scanner.Bytes()
		return event, nil
	}
	if err := e.scanner.Err(); err != nil {
		if err == context.Canceled {
			return nil, io.EOF
		}
		return nil, err
	}
	return nil, io.EOF
}
