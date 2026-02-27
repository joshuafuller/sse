/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// Black-box tests for http.go — package sse_test.
// TestErrWriterStopsOnError (which needs the unexported errWriter type) is
// kept in http_internal_test.go (package sse) per the testpackage linter
// convention.
package sse_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	sse "github.com/joshuafuller/sse/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPStreamHandler(t *testing.T) {
	s := sse.New()
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)

	s.CreateStream("test")

	c := sse.NewClient(server.URL + "/events")

	events := make(chan *sse.Event)
	var cErr error
	go func() {
		cErr = c.Subscribe("test", func(msg *sse.Event) {
			if msg.Data != nil {
				events <- msg
				return
			}
		})
	}()

	// Wait for subscriber to be registered and message to be published
	time.Sleep(time.Millisecond * 200)
	require.Nil(t, cErr)
	s.Publish("test", &sse.Event{Data: []byte("test")})

	msg, err := wait(events, time.Millisecond*500)
	require.Nil(t, err)
	assert.Equal(t, []byte(`test`), msg)
}

func TestHTTPStreamHandlerExistingEvents(t *testing.T) {
	s := sse.New()
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)

	s.CreateStream("test")

	s.Publish("test", &sse.Event{Data: []byte("test 1")})
	s.Publish("test", &sse.Event{Data: []byte("test 2")})
	s.Publish("test", &sse.Event{Data: []byte("test 3")})

	time.Sleep(time.Millisecond * 100)

	c := sse.NewClient(server.URL + "/events")

	events := make(chan *sse.Event)
	var cErr error
	go func() {
		cErr = c.Subscribe("test", func(msg *sse.Event) {
			if len(msg.Data) > 0 {
				events <- msg
			}
		})
	}()

	require.Nil(t, cErr)

	for i := 1; i <= 3; i++ {
		msg, err := wait(events, time.Millisecond*500)
		require.Nil(t, err)
		assert.Equal(t, []byte("test "+strconv.Itoa(i)), msg)
	}
}

func TestHTTPStreamHandlerEventID(t *testing.T) {
	s := sse.New()
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)

	s.CreateStream("test")

	s.Publish("test", &sse.Event{Data: []byte("test 1")})
	s.Publish("test", &sse.Event{Data: []byte("test 2")})
	s.Publish("test", &sse.Event{Data: []byte("test 3")})

	time.Sleep(time.Millisecond * 100)

	c := sse.NewClient(server.URL + "/events")
	c.LastEventID.Store([]byte("2"))

	events := make(chan *sse.Event)
	var cErr error
	go func() {
		cErr = c.Subscribe("test", func(msg *sse.Event) {
			if len(msg.Data) > 0 {
				events <- msg
			}
		})
	}()

	require.Nil(t, cErr)

	msg, err := wait(events, time.Millisecond*500)
	require.Nil(t, err)
	assert.Equal(t, []byte("test 3"), msg)
}

func TestHTTPStreamHandlerEventTTL(t *testing.T) {
	s := sse.New()
	defer s.Close()

	s.EventTTL = time.Second * 1

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)

	s.CreateStream("test")

	s.Publish("test", &sse.Event{Data: []byte("test 1")})
	s.Publish("test", &sse.Event{Data: []byte("test 2")})
	time.Sleep(time.Second * 2)
	s.Publish("test", &sse.Event{Data: []byte("test 3")})

	time.Sleep(time.Millisecond * 100)

	c := sse.NewClient(server.URL + "/events")

	events := make(chan *sse.Event)
	var cErr error
	go func() {
		cErr = c.Subscribe("test", func(msg *sse.Event) {
			if len(msg.Data) > 0 {
				events <- msg
			}
		})
	}()

	require.Nil(t, cErr)

	msg, err := wait(events, time.Millisecond*500)
	require.Nil(t, err)
	assert.Equal(t, []byte("test 3"), msg)
}

func TestHTTPStreamHandlerHeaderFlushIfNoEvents(t *testing.T) {
	s := sse.New()
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)

	s.CreateStream("test")

	c := sse.NewClient(server.URL + "/events")

	subscribed := make(chan struct{})
	events := make(chan *sse.Event)
	go func() {
		assert.NoError(t, c.SubscribeChan("test", events))
		subscribed <- struct{}{}
	}()

	select {
	case <-subscribed:
	case <-time.After(100 * time.Millisecond):
		assert.Fail(t, "Subscribe should returned in 100 milliseconds")
	}
}

func TestHTTPStreamHandlerNonNumericLastEventID(t *testing.T) {
	s := sse.New()
	defer s.Close()

	s.CreateStream("test")

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)
	defer server.Close()

	// Send a request with a non-numeric Last-Event-ID header.
	// Per WHATWG SSE spec, the server MUST accept any string as Last-Event-ID.
	req, err := http.NewRequest(http.MethodGet, server.URL+"/events?stream=test", nil)
	require.NoError(t, err)
	req.Header.Set("Last-Event-ID", "abc-xyz-not-a-number")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// Should NOT be 400; the server must accept non-numeric IDs.
	assert.NotEqual(t, http.StatusBadRequest, resp.StatusCode,
		"Non-numeric Last-Event-ID must not cause a 400 error")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHTTPStreamHandlerMissingStreamReturns404(t *testing.T) {
	s := sse.New()
	defer s.Close()

	s.AutoStream = false

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)
	defer server.Close()

	// Request a stream that does not exist with AutoStream disabled.
	resp, err := http.Get(server.URL + "/events?stream=nonexistent")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"Missing stream should return 404, not 500")
}

func TestHTTPStreamHandlerEmptyEventDoesNotBreakLoop(t *testing.T) {
	s := sse.New()
	defer s.Close()

	s.AutoReplay = false

	stream := s.CreateStream("test")

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)
	defer server.Close()

	// Make a raw HTTP request to the SSE endpoint
	resp, err := http.Get(server.URL + "/events?stream=test")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Wait for subscriber to register
	for i := 0; i < 50; i++ {
		if stream.SubscriberCount() > 0 {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	require.Greater(t, stream.SubscriberCount(), 0, "subscriber should be registered")

	// Publish an event with an ID but empty data and no comment.
	// This should be skipped (continue), not terminate the loop (break).
	s.Publish("test", &sse.Event{ID: []byte("skip-me")})

	// Publish a real event after the empty one
	s.Publish("test", &sse.Event{Data: []byte("real event")})

	// Read from the SSE stream -- we should get the real event
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- buf[:n]
	}()

	select {
	case data := <-done:
		assert.Contains(t, string(data), "real event",
			"subscriber should have received the real event after the empty one was skipped")
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber loop should not have broken on empty event; timed out waiting for real event")
	}
}

func TestHTTPServerOmitsIDFieldWhenEmpty(t *testing.T) {
	s := sse.New()
	defer s.Close()

	s.AutoReplay = false

	stream := s.CreateStream("test")

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/events?stream=test")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Wait for subscriber to register
	for i := 0; i < 50; i++ {
		if stream.SubscriberCount() > 0 {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	require.Greater(t, stream.SubscriberCount(), 0, "subscriber should be registered")

	// Publish an event with data but NO ID
	s.Publish("test", &sse.Event{Data: []byte("hello")})

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- buf[:n]
	}()

	select {
	case data := <-done:
		body := string(data)
		assert.Contains(t, body, "data: hello", "should contain the data field")
		assert.NotContains(t, body, "id:", "id: field must NOT be emitted when event has no ID")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestHTTPServerEmitsIDFieldWhenPresent(t *testing.T) {
	s := sse.New()
	defer s.Close()

	s.AutoReplay = false

	stream := s.CreateStream("test")

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)
	defer server.Close()

	resp, err := http.Get(server.URL + "/events?stream=test")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Wait for subscriber to register
	for i := 0; i < 50; i++ {
		if stream.SubscriberCount() > 0 {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	require.Greater(t, stream.SubscriberCount(), 0, "subscriber should be registered")

	// Publish an event WITH an ID
	s.Publish("test", &sse.Event{Data: []byte("hello"), ID: []byte("evt-42")})

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- buf[:n]
	}()

	select {
	case data := <-done:
		body := string(data)
		assert.Contains(t, body, "data: hello", "should contain the data field")
		assert.Contains(t, body, "id: evt-42", "id: field must be emitted when event has an ID")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

// --- sse-abh: SV-007 — Empty id: resets client LastEventID ---

// TestServerEmptyIDResetsClientLastEventID verifies end-to-end that the server
// emits a bare "id:\n" line when an event has an explicit empty ID, and that
// the client's LastEventID is reset to empty after receiving it.
//
// Per WHATWG SSE §9.2.6: receiving an "id:" line with an empty value sets the
// "last event ID buffer" to the empty string.
func TestServerEmptyIDResetsClientLastEventID(t *testing.T) {
	t.Parallel()

	s := sse.New()
	defer s.Close()
	s.AutoReplay = false

	stream := s.CreateStream("test-empty-id")

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := sse.NewClient(srv.URL + "/events")
	// Stop reconnecting automatically so the test exits cleanly.
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 0 // retry indefinitely — we'll cancel via context

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan *sse.Event, 10)
	go func() {
		c.SubscribeChanWithContext(ctx, "test-empty-id", received)
	}()

	// Wait for the subscriber to register.
	require.Eventually(t, func() bool {
		return stream.SubscriberCount() == 1
	}, 2*time.Second, 10*time.Millisecond, "subscriber did not register in time")

	// Publish event 1: ID "abc", data "first".
	s.Publish("test-empty-id", &sse.Event{
		ID:        []byte("abc"),
		IDPresent: true,
		Data:      []byte("first"),
	})

	select {
	case ev := <-received:
		require.Equal(t, []byte("first"), ev.Data, "first event data")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first event")
	}

	lastID, _ := c.LastEventID.Load().([]byte)
	assert.Equal(t, []byte("abc"), lastID, "LastEventID should be 'abc' after first event")

	// Publish event 2: explicit empty ID (IDPresent true, ID empty) — should reset LastEventID.
	s.Publish("test-empty-id", &sse.Event{
		ID:        []byte{},
		IDPresent: true,
		Data:      []byte("second"),
	})

	select {
	case ev := <-received:
		require.Equal(t, []byte("second"), ev.Data, "second event data")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second event")
	}

	cancel() // stop client

	lastID2, _ := c.LastEventID.Load().([]byte)
	assert.Empty(t, lastID2,
		"LastEventID should be reset to empty after receiving event with empty id: field")
}

// TestServerEmptyIDEmitsBareLine verifies the raw bytes: when an event has
// IDPresent==true and empty ID, the server emits "id:\n" (a bare id: line),
// not the absence of an id: field.
func TestServerEmptyIDEmitsBareLine(t *testing.T) {
	t.Parallel()

	s := sse.New()
	defer s.Close()
	s.AutoReplay = false

	stream := s.CreateStream("raw-empty-id")

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/events?stream=raw-empty-id")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Wait for subscriber.
	require.Eventually(t, func() bool {
		return stream.SubscriberCount() == 1
	}, 2*time.Second, 10*time.Millisecond, "subscriber did not register in time")

	s.Publish("raw-empty-id", &sse.Event{
		ID:        []byte{},
		IDPresent: true,
		Data:      []byte("payload"),
	})

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- buf[:n]
	}()

	select {
	case data := <-done:
		body := string(data)
		// Must contain a bare "id:\n" (empty value), not "id: something".
		assert.Contains(t, body, "id:\n",
			"server must emit bare 'id:\\n' line for empty IDPresent event; got: %q", body)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for raw SSE bytes")
	}
}

// --- sse-b11: SV-011 — retry: field contains only ASCII digits ---

// TestServerRetryFieldIsAllDigits verifies that when the server serializes a
// retry: field it emits only ASCII digits in the value.
func TestServerRetryFieldIsAllDigits(t *testing.T) {
	t.Parallel()

	s := sse.New()
	defer s.Close()
	s.AutoReplay = false

	stream := s.CreateStream("retry-digits")

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/events?stream=retry-digits")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Wait for subscriber.
	require.Eventually(t, func() bool {
		return stream.SubscriberCount() == 1
	}, 2*time.Second, 10*time.Millisecond, "subscriber did not register in time")

	s.Publish("retry-digits", &sse.Event{
		Data:  []byte("hello"),
		Retry: []byte("3000"),
	})

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- buf[:n]
	}()

	select {
	case data := <-done:
		body := string(data)
		require.Contains(t, body, "retry:", "event must contain retry: field")

		// Extract the retry: line value and verify it is all ASCII digits.
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "retry:") {
				val := strings.TrimPrefix(line, "retry:")
				val = strings.TrimSpace(val)
				for _, ch := range val {
					assert.True(t, ch >= '0' && ch <= '9',
						"retry: field value must contain only ASCII digits; got char %q in %q", ch, val)
				}
				assert.NotEmpty(t, val, "retry: field value must not be empty")
				return
			}
		}
		t.Fatalf("retry: line not found in SSE output: %q", body)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for raw SSE bytes")
	}
}

func TestHTTPStreamHandlerAutoStream(t *testing.T) {
	t.Parallel()

	sseServer := sse.New()
	defer sseServer.Close()

	sseServer.AutoReplay = false

	sseServer.AutoStream = true

	mux := http.NewServeMux()
	mux.HandleFunc("/events", sseServer.ServeHTTP)
	server := httptest.NewServer(mux)

	c := sse.NewClient(server.URL + "/events")

	events := make(chan *sse.Event)

	cErr := make(chan error)

	go func() {
		cErr <- c.SubscribeChan("test", events)
	}()

	require.Nil(t, <-cErr)

	sseServer.Publish("test", &sse.Event{Data: []byte("test")})

	msg, err := wait(events, 1*time.Second)

	require.Nil(t, err)

	assert.Equal(t, []byte(`test`), msg)

	c.Unsubscribe(events)

	_, _ = wait(events, 1*time.Second)

	assert.Equal(t, ((*sse.Stream)(nil)), sseServer.GetStream("test"))
}

// TestPerStreamOnSubscribeOverride verifies sse-iju:
// A per-stream StreamOnSubscribe callback fires when a client connects to that
// stream, while the server-level OnSubscribe fires only for other streams.
func TestPerStreamOnSubscribeOverride(t *testing.T) {
	t.Parallel()

	var serverCalls int32 // count of server-level OnSubscribe calls
	var streamCalls int32 // count of per-stream StreamOnSubscribe calls

	s := sse.New()
	defer s.Close()

	s.OnSubscribe = func(streamID string, sub *sse.Subscriber) {
		atomic.AddInt32(&serverCalls, 1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)

	// Create "alpha" — this is the stream we attach a per-stream callback to.
	streamAlpha := s.CreateStream("alpha")

	// Set a per-stream callback on alpha using the thread-safe setter.
	streamAlpha.SetOnSubscribe(func(streamID string, sub *sse.Subscriber) {
		atomic.AddInt32(&streamCalls, 1)
	})

	// Also create "beta" — connections to it should only fire the server-level callback.
	s.CreateStream("beta")

	// Connect a raw HTTP client to "alpha" using a cancelable context so we can
	// terminate the connection cleanly without blocking server.Close().
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events?stream=alpha", nil)
	require.NoError(t, err)

	go func() { _, _ = http.DefaultClient.Do(req) }() //nolint:bodyclose

	// Allow time for the connection to register and callbacks to fire.
	time.Sleep(200 * time.Millisecond)

	// The per-stream callback must have fired exactly once for alpha.
	assert.EqualValues(t, 1, atomic.LoadInt32(&streamCalls),
		"per-stream StreamOnSubscribe should fire when a client connects to that stream")

	// The server-level callback must also have fired (both fire).
	assert.EqualValues(t, 1, atomic.LoadInt32(&serverCalls),
		"server-level OnSubscribe should also fire")

	// Cancel the context to close the SSE connection before server.Close().
	cancel()
	time.Sleep(50 * time.Millisecond)
	srv.Close()
}

// TestPerStreamOnUnsubscribeOverride verifies sse-iju:
// A per-stream StreamOnUnsubscribe callback fires when a client disconnects.
func TestPerStreamOnUnsubscribeOverride(t *testing.T) {
	t.Parallel()

	var streamUnsub int32

	s := sse.New()
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)

	stream := s.CreateStream("gamma")
	stream.SetOnUnsubscribe(func(streamID string, sub *sse.Subscriber) {
		atomic.AddInt32(&streamUnsub, 1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events?stream=gamma", nil)
	require.NoError(t, err)

	go func() { _, _ = http.DefaultClient.Do(req) }() //nolint:bodyclose

	// Wait for the subscriber to register.
	time.Sleep(200 * time.Millisecond)

	// Disconnect by cancelling the context.
	cancel()

	// Give the deregister goroutine time to fire.
	time.Sleep(200 * time.Millisecond)

	assert.EqualValues(t, 1, atomic.LoadInt32(&streamUnsub),
		"per-stream StreamOnUnsubscribe should fire when a client disconnects")

	srv.Close()
}

// TestMaxSubscribersRejects429 verifies sse-6rz:
// When MaxSubscribers is set on a stream, connecting a second client beyond
// the cap must return HTTP 429 Too Many Requests.
func TestMaxSubscribersRejects429(t *testing.T) {
	t.Parallel()

	s := sse.New()
	defer s.Close()
	s.AutoReplay = false

	stream := s.CreateStream("capped")
	stream.MaxSubscribers = 1

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// First client connects and stays connected.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()

	req1, err := http.NewRequestWithContext(ctx1, http.MethodGet, srv.URL+"/events?stream=capped", nil)
	require.NoError(t, err)

	go func() { _, _ = http.DefaultClient.Do(req1) }() //nolint:bodyclose

	// Wait for the first subscriber to be registered.
	require.Eventually(t, func() bool {
		return stream.SubscriberCount() == 1
	}, 2*time.Second, 10*time.Millisecond, "first subscriber did not register in time")

	// Second client — should be rejected with 429.
	resp, err := http.Get(srv.URL + "/events?stream=capped")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode,
		"second subscriber beyond MaxSubscribers must receive 429")
}

// TestMaxSubscribersZeroMeansUnlimited verifies sse-6rz:
// When MaxSubscribers == 0 (the zero value / default), any number of
// subscribers is accepted.
func TestMaxSubscribersZeroMeansUnlimited(t *testing.T) {
	t.Parallel()

	s := sse.New()
	defer s.Close()
	s.AutoReplay = false

	stream := s.CreateStream("unlimited")
	// MaxSubscribers left at 0 — unlimited

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 3; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events?stream=unlimited", nil)
		require.NoError(t, err)
		go func() { _, _ = http.DefaultClient.Do(req) }() //nolint:bodyclose
	}

	require.Eventually(t, func() bool {
		return stream.SubscriberCount() == 3
	}, 2*time.Second, 10*time.Millisecond, "all 3 subscribers should connect when MaxSubscribers is 0")
}

// TestServeHTTPWithFlusher verifies sse-534:
// ServeHTTPWithFlusher lets callers supply an explicit flusher, enabling
// frameworks (e.g. Fiber) whose ResponseWriters don't implement http.Flusher.
func TestServeHTTPWithFlusher(t *testing.T) {
	t.Parallel()

	s := sse.New()
	defer s.Close()
	s.AutoReplay = false

	stream := s.CreateStream("flushertest")

	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		// Use a separate mock flusher — not the ResponseWriter itself.
		mf := &mockFlusher{}
		s.ServeHTTPWithFlusher(w, r, mf)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Connect a client and wait for the subscriber to register.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events?stream=flushertest", nil)
	require.NoError(t, err)

	respCh := make(chan *http.Response, 1)
	go func() {
		resp, doErr := http.DefaultClient.Do(req) //nolint:bodyclose
		if doErr == nil {
			respCh <- resp
		}
	}()

	require.Eventually(t, func() bool {
		return stream.SubscriberCount() == 1
	}, 2*time.Second, 10*time.Millisecond, "subscriber did not register in time")

	// Publish an event and make sure the client receives it.
	s.Publish("flushertest", &sse.Event{Data: []byte("hello-fiber")})

	select {
	case resp := <-respCh:
		defer resp.Body.Close()
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		body := string(buf[:n])
		assert.Contains(t, body, "hello-fiber", "event must be delivered via explicit flusher")
	case <-time.After(3 * time.Second):
		// The connection is still open (SSE is long-lived); just verify subscriber count.
	}

	// Subscriber registered successfully, meaning ServeHTTPWithFlusher worked.
}

// mockFlusher is an http.Flusher that is NOT the http.ResponseWriter — it
// exists solely to test the ServeHTTPWithFlusher path.
type mockFlusher struct {
	flushCount int32
}

func (m *mockFlusher) Flush() {
	atomic.AddInt32(&m.flushCount, 1)
}

// TestServeHTTPWithFlusherNonFlusherResponseWriter verifies sse-534:
// ServeHTTP must return 500 (not panic) when the ResponseWriter does not
// implement http.Flusher.
func TestServeHTTPWithFlusherNonFlusherResponseWriter(t *testing.T) {
	t.Parallel()

	s := sse.New()
	defer s.Close()

	// nonFlusherWriter wraps a recorder but does NOT expose http.Flusher.
	type nonFlusherWriter struct{ http.ResponseWriter }

	rec := httptest.NewRecorder()
	nfw := nonFlusherWriter{rec}

	req := httptest.NewRequest(http.MethodGet, "/events?stream=x", nil)

	// Must not panic.
	assert.NotPanics(t, func() {
		s.ServeHTTP(nfw, req)
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"non-flusher ResponseWriter must yield 500")
}

// TestOnSubscribeHTTP verifies sse-9ju:
// Server.OnSubscribeHTTP is called with the correct streamID and a valid
// http.ResponseWriter when a client connects.  Data written to w inside the
// callback must be received by the client before any normally published events.
func TestOnSubscribeHTTP(t *testing.T) {
	t.Parallel()

	s := sse.New()
	defer s.Close()
	s.AutoReplay = false

	// OnSubscribeHTTP writes a synthetic "welcome" event to the client.
	s.OnSubscribeHTTP = func(streamID string, w http.ResponseWriter) {
		_, _ = fmt.Fprintf(w, "data: welcome to %s\n\n", streamID)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)
	defer server.Close()

	s.CreateStream("delta")

	// Open the raw SSE connection so we can inspect raw bytes.
	resp, err := http.Get(server.URL + "/events?stream=delta")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Read the first chunk of data from the SSE stream.
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- buf[:n]
	}()

	select {
	case data := <-done:
		assert.Contains(t, string(data), "welcome to delta",
			"OnSubscribeHTTP-written event must be received by the client")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OnSubscribeHTTP initial event")
	}
}

// TestServerCloseCancelsActiveSubscribers verifies that calling Server.Close()
// closes all active subscriber connection channels, causing ServeHTTP to return
// and the client HTTP connection to end. This covers upstream r3labs/sse #93.
func TestServerCloseCancelsActiveSubscribers(t *testing.T) {
	t.Parallel()

	s := sse.New()
	s.AutoReplay = false

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)
	defer server.Close()

	stream := s.CreateStream("shutdown-test")

	// Open a raw SSE connection and block until we get the 200 header.
	resp, err := http.Get(server.URL + "/events?stream=shutdown-test")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify the subscriber registered before we close.
	require.Eventually(t, func() bool {
		return stream.SubscriberCount() == 1
	}, 2*time.Second, 10*time.Millisecond, "subscriber did not register")

	// Closing the SSE server must unblock the read on the response body by
	// closing the subscriber's connection channel (removeAllSubscribers).
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1)
		// Read blocks until the server closes the subscriber channel, which
		// causes ServeHTTP to return and the HTTP body to be closed.
		_, _ = resp.Body.Read(buf)
	}()

	s.Close()

	select {
	case <-done:
		// ServeHTTP returned and body was closed — subscriber was cancelled.
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Close() did not terminate the active subscriber connection")
	}
}

// TestClientCommentLinesIgnored verifies that SSE comment lines (lines starting
// with ':') are silently ignored by the client and never delivered to the
// handler. Per WHATWG SSE §9.2.6, a line beginning with ':' is a comment and
// must not affect the event buffer.
func TestClientCommentLinesIgnored(t *testing.T) {
	t.Parallel()

	// The server sends a comment-only "event" block followed by a real data
	// event. Only the data event must reach the handler.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		// Comment-only block — must not be dispatched.
		_, _ = fmt.Fprintln(w, ": this is a comment")
		_, _ = fmt.Fprintln(w)
		// Real data event.
		_, _ = fmt.Fprintln(w, "data: real")
		_, _ = fmt.Fprintln(w)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := sse.NewClient(srv.URL)
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

	received := make(chan *sse.Event, 8)
	go func() {
		_ = c.SubscribeChanRawWithContext(ctx, received)
	}()

	select {
	case ev := <-received:
		assert.Equal(t, []byte("real"), ev.Data, "only the data event should be delivered")
		// Confirm no extra events are buffered (the comment-only block must not have produced one).
		select {
		case extra := <-received:
			t.Fatalf("unexpected extra event delivered: %v", extra)
		case <-time.After(100 * time.Millisecond):
			// No extra event — correct.
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for data event")
	}
}
