/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// White-box tests for client.go that require access to unexported symbols
// (trimHeader, headerData, processEvent, startReadLoop, mu, retryDelay).
// Named *_internal_test.go per the testpackage linter convention so they
// are allowed to stay in package sse.
package sse

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrimHeader(t *testing.T) {
	tests := []struct {
		input []byte
		want  []byte
	}{
		{
			input: []byte("data: real data"),
			want:  []byte("real data"),
		},
		{
			input: []byte("data:real data"),
			want:  []byte("real data"),
		},
		{
			input: []byte("data:"),
			want:  []byte(""),
		},
	}

	for _, tc := range tests {
		got := trimHeader(len(headerData), tc.input)
		require.Equal(t, tc.want, got)
	}
}

// TestRetryFieldIgnoresNonNumeric verifies that a non-numeric retry: field
// value is silently ignored (retryDelay stays at zero).
func TestRetryFieldIgnoresNonNumeric(t *testing.T) {
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "retry: notanumber\ndata: hello\n\n")
		w.(http.Flusher).Flush()
	}))
	defer tsrv.Close()

	c := NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	done := make(chan struct{})
	go func() {
		c.SubscribeRaw(func(msg *Event) {
			close(done)
		})
	}()

	select {
	case <-done:
		// Event received successfully; non-numeric retry was ignored.
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}

	// retryDelay should be zero (non-numeric ignored).
	c.mu.Lock()
	assert.Equal(t, time.Duration(0), c.retryDelay, "non-numeric retry should be ignored")
	c.mu.Unlock()
}

// TestReadLoopDoesNotLeakOnConsumerExit verifies that readLoop does not
// block forever when the consumer exits without reading from erChan.
// With an unbuffered erChan, the goroutine would block on send and leak.
func TestReadLoopDoesNotLeakOnConsumerExit(t *testing.T) {
	c := NewClient("http://localhost")

	// A reader that immediately returns EOF.
	errReader := io.NopCloser(strings.NewReader(""))
	reader := NewEventStreamReader(errReader, 4096)

	before := runtime.NumGoroutine()

	// Use startReadLoop which creates the erChan internally.
	// Do NOT read from erChan at all — simulate consumer exit.
	c.startReadLoop(context.Background(), reader)

	// Give the goroutine time to complete (if buffered) or block (if unbuffered).
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	// If erChan is buffered, the goroutine completes and we should see no growth.
	// With unbuffered erChan, the goroutine is stuck and we see +1.
	assert.LessOrEqual(t, after, before,
		"readLoop goroutine should not leak when consumer does not read erChan")
}

// TestEventStreamReaderStripsLeadingBOM verifies that a leading UTF-8 BOM
// (EF BB BF) is stripped before processing, per WHATWG SSE §9.2.6.
func TestEventStreamReaderStripsLeadingBOM(t *testing.T) {
	// BOM followed by a normal SSE event.
	input := "\xEF\xBB\xBFdata: hello\n\n"
	reader := NewEventStreamReader(strings.NewReader(input), 4096)
	eventBytes, err := reader.ReadEvent()
	require.NoError(t, err)

	c := NewClient("http://localhost")
	event, err := c.processEvent(eventBytes)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), event.Data, "BOM should be stripped; data must parse cleanly")
}

// TestEventStreamReaderNoBOM verifies normal operation without a BOM.
func TestEventStreamReaderNoBOM(t *testing.T) {
	input := "data: world\n\n"
	reader := NewEventStreamReader(strings.NewReader(input), 4096)
	eventBytes, err := reader.ReadEvent()
	require.NoError(t, err)

	c := NewClient("http://localhost")
	event, err := c.processEvent(eventBytes)
	require.NoError(t, err)
	assert.Equal(t, []byte("world"), event.Data)
}

// slowReader is an io.ReadCloser that produces one SSE event per tick, pausing
// between events. It simulates a long-lived stream that keeps sending events
// so that readLoop keeps looping. On Close() it unblocks all pending reads.
type slowReader struct {
	events []string
	idx    int
	tick   time.Duration
	mu     sync.Mutex
	closed chan struct{}
	buf    bytes.Buffer
}

func newSlowReader(events []string, tick time.Duration) *slowReader {
	return &slowReader{events: events, tick: tick, closed: make(chan struct{})}
}

func (s *slowReader) Read(p []byte) (int, error) {
	// If buffered data remains, serve it.
	s.mu.Lock()
	if s.buf.Len() > 0 {
		n, err := s.buf.Read(p)
		s.mu.Unlock()
		return n, err
	}
	s.mu.Unlock()

	// Wait tick or close.
	select {
	case <-s.closed:
		return 0, io.EOF
	case <-time.After(s.tick):
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx < len(s.events) {
		s.buf.WriteString(s.events[s.idx])
		s.idx++
	} else {
		// No more events; block indefinitely (simulates idle stream).
		s.mu.Unlock()
		<-s.closed
		s.mu.Lock()
		return 0, io.EOF
	}
	return s.buf.Read(p)
}

func (s *slowReader) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

// TestReadLoopExitsOnContextCancel verifies that after context cancellation the
// readLoop goroutine sends context.Canceled to erChan and exits. The goroutine
// is checked via the post-event ctx.Done() select that runs after each
// successfully dispatched event.
func TestReadLoopExitsOnContextCancel(t *testing.T) {
	c := NewClient("http://localhost")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Events stream: two events then the reader blocks forever.
	// We cancel the context after the first event arrives so that
	// readLoop exits via the post-event ctx.Done() check.
	sr := newSlowReader([]string{
		"data: first\n\n",
		"data: second\n\n",
	}, 20*time.Millisecond)
	defer sr.Close()

	reader := NewEventStreamReader(sr, 4096)
	outCh, erChan := c.startReadLoop(ctx, reader)

	// Wait for readLoop to have an event ready to send (it blocks on outCh <- msg
	// until we receive). Cancel the context BEFORE receiving so that by the time
	// outCh unblocks readLoop, the post-event ctx.Done() check fires.
	//
	// Give the reader time to produce the first event.
	time.Sleep(100 * time.Millisecond)

	// Cancel the context while readLoop is blocked sending the first event.
	cancel()

	// Now receive the event — this unblocks readLoop's outCh send.
	select {
	case <-outCh:
		// first event received
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first event")
	}

	// readLoop should detect ctx.Done() in the post-event check and send
	// context.Canceled to erChan promptly.
	select {
	case err := <-erChan:
		assert.ErrorIs(t, err, context.Canceled,
			"expected context.Canceled from readLoop on ctx cancel, got: %v", err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("readLoop did not exit within 500ms of context cancellation")
	}
}

// TestUnexpectedEOFDoesNotFireDisconnect verifies that when readLoop receives
// io.ErrUnexpectedEOF the disconnect callback is NOT called, but the error IS
// forwarded to erChan so that backoff can trigger a reconnect.
func TestUnexpectedEOFDoesNotFireDisconnect(t *testing.T) {
	c := NewClient("http://localhost")

	disconnectCalled := make(chan struct{}, 1)
	c.OnDisconnect(func(cl *Client) {
		select {
		case disconnectCalled <- struct{}{}:
		default:
		}
	})

	// A reader that immediately returns io.ErrUnexpectedEOF.
	pr, pw := io.Pipe()
	_ = pw.CloseWithError(io.ErrUnexpectedEOF)

	reader := NewEventStreamReader(pr, 4096)
	_, erChan := c.startReadLoop(context.Background(), reader)

	// erChan must receive the error (reconnect path).
	select {
	case err := <-erChan:
		assert.Equal(t, io.ErrUnexpectedEOF, err, "erChan must receive io.ErrUnexpectedEOF")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for erChan to receive io.ErrUnexpectedEOF")
	}

	// disconnectcb must NOT have been called.
	select {
	case <-disconnectCalled:
		t.Fatal("disconnectcb must not be called for io.ErrUnexpectedEOF")
	case <-time.After(50 * time.Millisecond):
		// good
	}
}

// TestProcessEventCRLF verifies that CRLF (\r\n) is treated as a SINGLE line
// ending per WHATWG SSE §9.2.6. The bug: bytes.FieldsFunc splits on \r and \n
// independently, so \r\n produces an empty token between them that may corrupt
// field parsing or produce ghost empty lines.
func TestProcessEventCRLF(t *testing.T) {
	c := NewClient("http://localhost")

	// "data: hello\r\n" is a single field line terminated by CRLF.
	raw := []byte("data: hello\r\n")
	event, err := c.processEvent(raw)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), event.Data,
		"CRLF line ending must be treated as single line ending; got Data=%q", event.Data)
}

// TestProcessEventBareCR verifies that a bare \r is treated as a single line
// ending, per WHATWG SSE §9.2.6.
func TestProcessEventBareCR(t *testing.T) {
	c := NewClient("http://localhost")

	// "data: hello\r" — bare CR terminates the field line.
	raw := []byte("data: hello\r")
	event, err := c.processEvent(raw)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), event.Data,
		"bare CR must be treated as a single line ending; got Data=%q", event.Data)
}

// TestProcessEventMixedLineEndings verifies that a single event block
// containing both CRLF and LF-terminated field lines parses all fields
// correctly. Mixed line endings appear in the wild.
func TestProcessEventMixedLineEndings(t *testing.T) {
	c := NewClient("http://localhost")

	// Two fields: event type on CRLF, data on LF.
	raw := []byte("event: foo\r\ndata: bar\n")
	event, err := c.processEvent(raw)
	require.NoError(t, err)
	assert.Equal(t, []byte("foo"), event.Event,
		"event field must parse from CRLF-terminated line; got Event=%q", event.Event)
	assert.Equal(t, []byte("bar"), event.Data,
		"data field must parse from LF-terminated line; got Data=%q", event.Data)
}

// TestProcessEventCRLFMultipleDataLines verifies that multiple data: fields
// separated by CRLF are concatenated correctly (with \n between them) per spec.
func TestProcessEventCRLFMultipleDataLines(t *testing.T) {
	c := NewClient("http://localhost")

	// Two data lines with CRLF — must concatenate to "line1\nline2".
	raw := []byte("data: line1\r\ndata: line2\r\n")
	event, err := c.processEvent(raw)
	require.NoError(t, err)
	assert.Equal(t, []byte("line1\nline2"), event.Data,
		"multiple CRLF-separated data: lines must concatenate with \\n; got Data=%q", event.Data)
}

// TestProcessEventCRLFDoesNotProduceSpuriousEmptyFields ensures that a
// CRLF sequence does NOT produce an empty intermediate token that would be
// misidentified as a bare "data" line (triggering an empty data append).
func TestProcessEventCRLFDoesNotProduceSpuriousEmptyFields(t *testing.T) {
	c := NewClient("http://localhost")

	raw := []byte("data: x\r\nevent: y\r\n")
	event, err := c.processEvent(raw)
	require.NoError(t, err)
	// Data must be exactly "x" — no extra \n from a phantom empty line.
	assert.Equal(t, []byte("x"), event.Data,
		"CRLF must not produce empty intermediate tokens; got Data=%q", event.Data)
	assert.Equal(t, []byte("y"), event.Event,
		"event field must parse correctly; got Event=%q", event.Event)
}

// TestEventStreamReaderCRLFSingleLineEnding verifies the end-to-end path:
// EventStreamReader correctly delivers an event whose fields are CRLF-terminated,
// and processEvent then parses it cleanly into the correct field values.
func TestEventStreamReaderCRLFSingleLineEnding(t *testing.T) {
	input := "data: hello\r\nevent: greet\r\n"
	full := input + "\r\n"
	reader := NewEventStreamReader(strings.NewReader(full), 4096)
	eventBytes, err := reader.ReadEvent()
	require.NoError(t, err)

	c := NewClient("http://localhost")
	event, err := c.processEvent(eventBytes)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), event.Data,
		"end-to-end CRLF: Data must be 'hello'; got %q", event.Data)
	assert.Equal(t, []byte("greet"), event.Event,
		"end-to-end CRLF: Event must be 'greet'; got %q", event.Event)
}

// TestEventStreamReaderCRLFBareCRTerminator verifies that an event stream using
// CRLF field-line endings with a bare-CR event terminator (\r\n\r) correctly
// splits two back-to-back events.
// Regression test for sse-frd WP-004 scanner gap.
func TestEventStreamReaderCRLFBareCRTerminator(t *testing.T) {
	full := "data: first\r\n\r" + "data: second\n\n"
	reader := NewEventStreamReader(strings.NewReader(full), 4096)

	// First event.
	eventBytes1, err := reader.ReadEvent()
	require.NoError(t, err, "first ReadEvent must succeed")
	require.NotEmpty(t, eventBytes1, "first ReadEvent must return non-empty bytes")

	c := NewClient("http://localhost")
	ev1, err := c.processEvent(eventBytes1)
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), ev1.Data,
		"first event Data must be 'first'; got %q", ev1.Data)

	// Second event — must be a separate event, NOT merged with the first.
	eventBytes2, err := reader.ReadEvent()
	require.NoError(t, err, "second ReadEvent must succeed")
	require.NotEmpty(t, eventBytes2, "second ReadEvent must return non-empty bytes")

	ev2, err := c.processEvent(eventBytes2)
	require.NoError(t, err)
	assert.Equal(t, []byte("second"), ev2.Data,
		"second event Data must be 'second'; got %q (events must not be merged)", ev2.Data)
}

// TestTrimHeaderShortData verifies that trimHeader returns the slice unchanged
// when len(data) < size (the early-return guard).
func TestTrimHeaderShortData(t *testing.T) {
	// headerData is "data: " — 6 bytes. Pass only 3 bytes; must be returned as-is.
	short := []byte("dat")
	got := trimHeader(len(headerData), short)
	require.Equal(t, short, got, "trimHeader must return slice unchanged when len(data) < size")
}

// TestProcessEventBase64Valid verifies that processEvent with EncodingBase64=true
// correctly base64-decodes the event data.
func TestProcessEventBase64Valid(t *testing.T) {
	// base64("hello") = "aGVsbG8="
	raw := []byte("data: aGVsbG8=\n")
	c := NewClient("http://localhost")
	c.EncodingBase64 = true

	event, err := c.processEvent(raw)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), event.Data,
		"processEvent with EncodingBase64=true must decode base64 data; got %q", event.Data)
}

// TestProcessEventBase64Invalid verifies that processEvent with EncodingBase64=true
// returns an error that WRAPS the underlying decode error (i.e. errors.As works).
// This test fails before the %s→%w fix in client.go.
func TestProcessEventBase64Invalid(t *testing.T) {
	// "!!!!" is not valid base64.
	raw := []byte("data: !!!!\n")
	c := NewClient("http://localhost")
	c.EncodingBase64 = true

	_, err := c.processEvent(raw)
	require.Error(t, err, "processEvent must return an error for invalid base64")

	// The error must WRAP the underlying base64 CorruptInputError so that
	// errors.As can unwrap it. With %s the error is a string copy and
	// errors.As returns false.
	var corruptErr base64.CorruptInputError
	assert.True(t, errors.As(err, &corruptErr),
		"error must wrap the base64 CorruptInputError (requires %%w, not %%s); got: %v", err)
}

// TestConnectedFieldRace verifies that c.Connected is safe for concurrent
// access.  The race detector will fire here before the fix (plain bool with
// no synchronisation) and pass cleanly afterwards (atomic.Bool).
//
// Scenario:
//   - readLoop goroutine writes c.Connected = false (disconnect path)
//   - SubscribeChanWithContext goroutine reads AND writes c.Connected (connect
//     path) on each attempt
//
// We replicate the read/write directly on the internal field so the test runs
// quickly without a real network, and we run several goroutines in parallel so
// the race detector has a chance to catch the unsynchronised access.
func TestConnectedFieldRace(t *testing.T) {
	c := NewClient("http://localhost") // no real connection needed

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Writer goroutines: simulate readLoop writing connected=false.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.connected.Store(false)
			}
		}()
	}

	// Reader+writer goroutines: simulate the connect path reading then writing.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.connected.CompareAndSwap(false, true)
			}
		}()
	}

	wg.Wait()
}

// TestEventStreamReaderLFBareCRTerminator verifies that an event stream using
// LF field-line endings with a bare-CR event terminator (\n\r) correctly splits
// two back-to-back events.
// Regression test for sse-frd WP-004 scanner gap.
func TestEventStreamReaderLFBareCRTerminator(t *testing.T) {
	full := "data: first\n\r" + "data: second\n\n"
	reader := NewEventStreamReader(strings.NewReader(full), 4096)

	// First event.
	eventBytes1, err := reader.ReadEvent()
	require.NoError(t, err, "first ReadEvent must succeed")
	require.NotEmpty(t, eventBytes1, "first ReadEvent must return non-empty bytes")

	c := NewClient("http://localhost")
	ev1, err := c.processEvent(eventBytes1)
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), ev1.Data,
		"first event Data must be 'first'; got %q", ev1.Data)

	// Second event — must be a separate event, NOT merged with the first.
	eventBytes2, err := reader.ReadEvent()
	require.NoError(t, err, "second ReadEvent must succeed")
	require.NotEmpty(t, eventBytes2, "second ReadEvent must return non-empty bytes")

	ev2, err := c.processEvent(eventBytes2)
	require.NoError(t, err)
	assert.Equal(t, []byte("second"), ev2.Data,
		"second event Data must be 'second'; got %q (events must not be merged)", ev2.Data)
}
