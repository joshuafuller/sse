/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errAfterNWriter is a ResponseWriter that returns an error after n bytes.
type errAfterNWriter struct {
	http.ResponseWriter
	remaining int
}

func (e *errAfterNWriter) Write(p []byte) (int, error) {
	if len(p) > e.remaining {
		e.remaining = 0
		return 0, errors.New("write error: connection closed")
	}
	e.remaining -= len(p)
	return e.ResponseWriter.Write(p)
}

func TestHTTPStreamHandler(t *testing.T) {
	s := New()
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)

	s.CreateStream("test")

	c := NewClient(server.URL + "/events")

	events := make(chan *Event)
	var cErr error
	go func() {
		cErr = c.Subscribe("test", func(msg *Event) {
			if msg.Data != nil {
				events <- msg
				return
			}
		})
	}()

	// Wait for subscriber to be registered and message to be published
	time.Sleep(time.Millisecond * 200)
	require.Nil(t, cErr)
	s.Publish("test", &Event{Data: []byte("test")})

	msg, err := wait(events, time.Millisecond*500)
	require.Nil(t, err)
	assert.Equal(t, []byte(`test`), msg)
}

func TestHTTPStreamHandlerExistingEvents(t *testing.T) {
	s := New()
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)

	s.CreateStream("test")

	s.Publish("test", &Event{Data: []byte("test 1")})
	s.Publish("test", &Event{Data: []byte("test 2")})
	s.Publish("test", &Event{Data: []byte("test 3")})

	time.Sleep(time.Millisecond * 100)

	c := NewClient(server.URL + "/events")

	events := make(chan *Event)
	var cErr error
	go func() {
		cErr = c.Subscribe("test", func(msg *Event) {
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
	s := New()
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)

	s.CreateStream("test")

	s.Publish("test", &Event{Data: []byte("test 1")})
	s.Publish("test", &Event{Data: []byte("test 2")})
	s.Publish("test", &Event{Data: []byte("test 3")})

	time.Sleep(time.Millisecond * 100)

	c := NewClient(server.URL + "/events")
	c.LastEventID.Store([]byte("2"))

	events := make(chan *Event)
	var cErr error
	go func() {
		cErr = c.Subscribe("test", func(msg *Event) {
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
	s := New()
	defer s.Close()

	s.EventTTL = time.Second * 1

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)

	s.CreateStream("test")

	s.Publish("test", &Event{Data: []byte("test 1")})
	s.Publish("test", &Event{Data: []byte("test 2")})
	time.Sleep(time.Second * 2)
	s.Publish("test", &Event{Data: []byte("test 3")})

	time.Sleep(time.Millisecond * 100)

	c := NewClient(server.URL + "/events")

	events := make(chan *Event)
	var cErr error
	go func() {
		cErr = c.Subscribe("test", func(msg *Event) {
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
	s := New()
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)

	s.CreateStream("test")

	c := NewClient(server.URL + "/events")

	subscribed := make(chan struct{})
	events := make(chan *Event)
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
	s := New()
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
	defer resp.Body.Close()

	// Should NOT be 400; the server must accept non-numeric IDs.
	assert.NotEqual(t, http.StatusBadRequest, resp.StatusCode,
		"Non-numeric Last-Event-ID must not cause a 400 error")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHTTPStreamHandlerMissingStreamReturns404(t *testing.T) {
	s := New()
	defer s.Close()

	s.AutoStream = false

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	server := httptest.NewServer(mux)
	defer server.Close()

	// Request a stream that does not exist with AutoStream disabled.
	resp, err := http.Get(server.URL + "/events?stream=nonexistent")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"Missing stream should return 404, not 500")
}

func TestHTTPStreamHandlerEmptyEventDoesNotBreakLoop(t *testing.T) {
	s := New()
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
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Wait for subscriber to register
	for i := 0; i < 50; i++ {
		if stream.getSubscriberCount() > 0 {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	require.Greater(t, stream.getSubscriberCount(), 0, "subscriber should be registered")

	// Publish an event with an ID but empty data and no comment.
	// This should be skipped (continue), not terminate the loop (break).
	s.Publish("test", &Event{ID: []byte("skip-me")})

	// Publish a real event after the empty one
	s.Publish("test", &Event{Data: []byte("real event")})

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
	s := New()
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
		if stream.getSubscriberCount() > 0 {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	require.Greater(t, stream.getSubscriberCount(), 0, "subscriber should be registered")

	// Publish an event with data but NO ID
	s.Publish("test", &Event{Data: []byte("hello")})

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
	s := New()
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
		if stream.getSubscriberCount() > 0 {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	require.Greater(t, stream.getSubscriberCount(), 0, "subscriber should be registered")

	// Publish an event WITH an ID
	s.Publish("test", &Event{Data: []byte("hello"), ID: []byte("evt-42")})

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

func TestErrWriterStopsOnError(t *testing.T) {
	t.Parallel()

	w := &errAfterNWriter{
		ResponseWriter: httptest.NewRecorder(),
		remaining:      10, // allow 10 bytes then fail
	}

	ew := &errWriter{w: w}

	// First write should succeed (fits in 10 bytes)
	ew.printf("hi")
	assert.Nil(t, ew.err, "first write should succeed")

	// This write should fail (exceeds remaining bytes)
	ew.printf("this is a long string that exceeds the limit")
	assert.NotNil(t, ew.err, "write past limit should fail")

	prevErr := ew.err

	// Subsequent writes should be no-ops
	ew.printf("more data")
	assert.Equal(t, prevErr, ew.err, "error should not change after first failure")

	ew.print("yet more")
	assert.Equal(t, prevErr, ew.err, "print should also be a no-op after error")
}

func TestHTTPStreamHandlerAutoStream(t *testing.T) {
	t.Parallel()

	sseServer := New()
	defer sseServer.Close()

	sseServer.AutoReplay = false

	sseServer.AutoStream = true

	mux := http.NewServeMux()
	mux.HandleFunc("/events", sseServer.ServeHTTP)
	server := httptest.NewServer(mux)

	c := NewClient(server.URL + "/events")

	events := make(chan *Event)

	cErr := make(chan error)

	go func() {
		cErr <- c.SubscribeChan("test", events)
	}()

	require.Nil(t, <-cErr)

	sseServer.Publish("test", &Event{Data: []byte("test")})

	msg, err := wait(events, 1*time.Second)

	require.Nil(t, err)

	assert.Equal(t, []byte(`test`), msg)

	c.Unsubscribe(events)

	_, _ = wait(events, 1*time.Second)

	assert.Equal(t, (*Stream)(nil), sseServer.getStream("test"))
}

// TestPerStreamOnSubscribeOverride verifies sse-iju:
// A per-stream StreamOnSubscribe callback fires when a client connects to that
// stream, while the server-level OnSubscribe fires only for other streams.
func TestPerStreamOnSubscribeOverride(t *testing.T) {
	t.Parallel()

	var serverCalls int32 // count of server-level OnSubscribe calls
	var streamCalls int32 // count of per-stream StreamOnSubscribe calls

	s := New()
	defer s.Close()

	s.OnSubscribe = func(streamID string, sub *Subscriber) {
		atomic.AddInt32(&serverCalls, 1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)

	// Create "alpha" — this is the stream we attach a per-stream callback to.
	streamAlpha := s.CreateStream("alpha")

	// Set a per-stream callback on alpha using the thread-safe setter.
	streamAlpha.SetOnSubscribe(func(streamID string, sub *Subscriber) {
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

	s := New()
	defer s.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", s.ServeHTTP)
	srv := httptest.NewServer(mux)

	stream := s.CreateStream("gamma")
	stream.SetOnUnsubscribe(func(streamID string, sub *Subscriber) {
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

// TestOnSubscribeHTTP verifies sse-9ju:
// Server.OnSubscribeHTTP is called with the correct streamID and a valid
// http.ResponseWriter when a client connects.  Data written to w inside the
// callback must be received by the client before any normally published events.
func TestOnSubscribeHTTP(t *testing.T) {
	t.Parallel()

	s := New()
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
