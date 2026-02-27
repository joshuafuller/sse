/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	backoff "github.com/cenkalti/backoff/v4"
)

var urlPath string
var srv *Server
var server *httptest.Server

var mldata = `{
	"key": "value",
	"array": [
		1,
		2,
		3
	]
}`

func setup(empty bool) {
	// New Server
	srv = newServer()
	// Send almost-continuous string of events to the client
	go publishMsgs(srv, empty, 100000000)
}

func setupMultiline() {
	srv = newServer()
	srv.SplitData = true
	go publishMultilineMessages(srv, 100000000)
}

func setupCount(empty bool, count int) {
	srv = newServer()
	go publishMsgs(srv, empty, count)
}

func newServer() *Server {
	srv = New()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", srv.ServeHTTP)
	server = httptest.NewServer(mux)
	urlPath = server.URL + "/events"

	srv.CreateStream("test")

	return srv
}

func newServer401() *Server {
	srv = New()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	server = httptest.NewServer(mux)
	urlPath = server.URL + "/events"

	srv.CreateStream("test")

	return srv
}

func publishMsgs(s *Server, empty bool, count int) {
	for a := 0; a < count; a++ {
		if empty {
			s.Publish("test", &Event{Data: []byte("\n")})
		} else {
			s.Publish("test", &Event{Data: []byte("ping")})
		}
		time.Sleep(time.Millisecond * 50)
	}
}

func publishMultilineMessages(s *Server, count int) {
	for a := 0; a < count; a++ {
		s.Publish("test", &Event{ID: []byte("123456"), Data: []byte(mldata)})
	}
}

func cleanup() {
	server.CloseClientConnections()
	server.Close()
	srv.Close()
}

func TestClientSubscribe(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

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

	for i := 0; i < 5; i++ {
		msg, err := wait(events, time.Second*1)
		require.Nil(t, err)
		assert.Equal(t, []byte(`ping`), msg)
	}

	assert.Nil(t, cErr)
}

func TestClientSubscribeMultiline(t *testing.T) {
	setupMultiline()
	defer cleanup()

	c := NewClient(urlPath)

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

	for i := 0; i < 5; i++ {
		msg, err := wait(events, time.Second*1)
		require.Nil(t, err)
		assert.Equal(t, []byte(mldata), msg)
	}

	assert.Nil(t, cErr)
}

func TestClientChanSubscribeEmptyMessage(t *testing.T) {
	// Per WHATWG spec: events with empty data (even if the server assigns
	// an ID) must NOT be dispatched to the client. Verify that no events
	// arrive within a reasonable window.
	setup(true)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	select {
	case ev := <-events:
		t.Fatalf("expected no events for empty data, got ID=%q Data=%q", ev.ID, ev.Data)
	case <-time.After(300 * time.Millisecond):
		// good — no events dispatched
	}
	c.Unsubscribe(events)
}

func TestClientChanSubscribe(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	for i := 0; i < 5; i++ {
		msg, merr := wait(events, time.Second*1)
		if msg == nil {
			i--
			continue
		}
		assert.Nil(t, merr)
		assert.Equal(t, []byte(`ping`), msg)
	}
	c.Unsubscribe(events)
}

func TestClientOnDisconnect(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	called := make(chan struct{})
	c.OnDisconnect(func(client *Client) {
		called <- struct{}{}
	})

	go c.Subscribe("test", func(msg *Event) {})

	time.Sleep(time.Second)
	server.CloseClientConnections()

	assert.Equal(t, struct{}{}, <-called)
}

func TestClientOnConnect(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	called := make(chan struct{})
	c.OnConnect(func(client *Client) {
		called <- struct{}{}
	})

	go c.Subscribe("test", func(msg *Event) {})

	time.Sleep(time.Second)
	assert.Equal(t, struct{}{}, <-called)

	server.CloseClientConnections()
}

func TestClientChanReconnect(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	for i := 0; i < 10; i++ {
		if i == 5 {
			// kill connection
			server.CloseClientConnections()
		}
		msg, merr := wait(events, time.Second*1)
		if msg == nil {
			i--
			continue
		}
		assert.Nil(t, merr)
		assert.Equal(t, []byte(`ping`), msg)
	}
	c.Unsubscribe(events)
}

func TestClientUnsubscribe(t *testing.T) {
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	time.Sleep(time.Millisecond * 500)

	go c.Unsubscribe(events)
	go c.Unsubscribe(events)
}

func TestClientUnsubscribeNonBlock(t *testing.T) {
	count := 2
	setupCount(false, count)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	// Read count messages from the channel
	for i := 0; i < count; i++ {
		msg, merr := wait(events, time.Second*1)
		assert.Nil(t, merr)
		assert.Equal(t, []byte(`ping`), msg)
	}
	// No more data is available to be read in the channel
	// Make sure Unsubscribe returns quickly
	doneCh := make(chan *Event)
	go func() {
		var e Event
		c.Unsubscribe(events)
		doneCh <- &e
	}()
	_, merr := wait(doneCh, time.Millisecond*100)
	assert.Nil(t, merr)
}

func TestClientUnsubscribe401(t *testing.T) {
	srv = newServer401()
	defer cleanup()

	c := NewClient(urlPath)

	// limit retries to 3
	c.ReconnectStrategy = backoff.WithMaxRetries(
		backoff.NewExponentialBackOff(),
		3,
	)

	err := c.SubscribeRaw(func(ev *Event) {
		// this shouldn't run
		assert.False(t, true)
	})

	require.NotNil(t, err)
}

func TestClient204NoReconnect(t *testing.T) {
	// HTTP 204 must stop reconnection immediately — spec §9.2.3.
	var requests int
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer tsrv.Close()

	c := NewClient(tsrv.URL)
	err := c.SubscribeRaw(func(ev *Event) {})

	require.Error(t, err)
	assert.Equal(t, 1, requests, "client must not reconnect after 204")
}

func TestClientLargeData(t *testing.T) {
	srv = newServer()
	defer cleanup()

	c := NewClient(urlPath, ClientMaxBufferSize(1<<19))

	// limit retries to 3
	c.ReconnectStrategy = backoff.WithMaxRetries(
		backoff.NewExponentialBackOff(),
		3,
	)

	// allocate 128KB of data to send
	data := make([]byte, 1<<17)
	rand.Read(data)
	data = []byte(hex.EncodeToString(data))

	ec := make(chan *Event, 1)

	srv.Publish("test", &Event{Data: data})

	go func() {
		c.Subscribe("test", func(ev *Event) {
			ec <- ev
		})
	}()

	d, err := wait(ec, time.Second)
	require.Nil(t, err)
	require.Equal(t, data, d)
}

func TestClientComment(t *testing.T) {
	srv = newServer()
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	srv.Publish("test", &Event{Comment: []byte("comment")})
	srv.Publish("test", &Event{Data: []byte("test")})

	ev, err := waitEvent(events, time.Second*1)
	assert.Nil(t, err)
	assert.Equal(t, []byte("test"), ev.Data)

	c.Unsubscribe(events)
}

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

func TestSubscribeWithContextDone(t *testing.T) {
	// Use an isolated server that streams events on demand so we don't
	// depend on the global setup/cleanup helpers or their timing.
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		// Stream events until the client disconnects.
		for {
			_, err := fmt.Fprint(w, "data: ping\n\n")
			if err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer tsrv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	const numSubs = 10
	// connected confirms each goroutine has received at least one event,
	// proving it is fully subscribed before we cancel the context.
	var connected sync.WaitGroup
	connected.Add(numSubs)

	// done tracks when each SubscribeWithContext call returns.
	var done sync.WaitGroup
	done.Add(numSubs)

	c := NewClient(tsrv.URL)

	for i := 0; i < numSubs; i++ {
		go func() {
			defer done.Done()
			once := sync.Once{}
			c.SubscribeWithContext(ctx, "", func(msg *Event) {
				once.Do(func() { connected.Done() })
			})
		}()
	}

	// Wait for all goroutines to be fully subscribed (event-driven, not time-based).
	connected.Wait()
	cancel()

	// Wait for all SubscribeWithContext calls to return after context cancellation.
	doneCh := make(chan struct{})
	go func() { done.Wait(); close(doneCh) }()

	select {
	case <-doneCh:
		// All goroutines exited — no leak.
	case <-time.After(5 * time.Second):
		t.Fatal("goroutines spawned by SubscribeWithContext did not exit after context cancellation")
	}
}

func TestResponseBodyClosedOnValidatorError(t *testing.T) {
	closeCalled := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	client.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)
	client.ResponseValidator = func(c *Client, resp *http.Response) error {
		original := resp.Body
		resp.Body = &bodyCloseTracker{ReadCloser: original, closed: closeCalled}
		return fmt.Errorf("validator rejected response")
	}

	err := client.Subscribe("", func(msg *Event) {})
	assert.Error(t, err)

	select {
	case <-closeCalled:
		// good
	case <-time.After(time.Second):
		t.Fatal("response body was not closed after validator error")
	}
}

func TestClientReconnectsAfterEOF(t *testing.T) {
	// Server sends one event per connection then closes cleanly (EOF).
	// Client must reconnect and receive a second event from the next connection.
	var mu sync.Mutex
	conns := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns++
		n := conns
		mu.Unlock()

		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: conn%d\n\n", n)
		flusher.Flush()
		// Returning from the handler closes the response body → client receives EOF.
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stop the subscription goroutine when the test ends

	c := NewClient(srv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 5)

	events := make(chan *Event, 10)
	go c.SubscribeRawWithContext(ctx, func(msg *Event) {
		events <- msg
	})

	ev1, err := waitEvent(events, 2*time.Second)
	require.Nil(t, err, "timed out waiting for first event")
	assert.Equal(t, []byte("conn1"), ev1.Data)

	// Without the fix, this blocks forever — the client does not reconnect after EOF.
	ev2, err := waitEvent(events, 2*time.Second)
	require.Nil(t, err, "timed out waiting for second event — client did not reconnect after EOF")
	assert.Equal(t, []byte("conn2"), ev2.Data)
}

func TestClientUnsubscribeWhileBackingOff(t *testing.T) {
	// When the client is in a reconnect backoff sleep (between retries),
	// Unsubscribe must return immediately — not block until the sleep expires.
	//
	// Use an isolated server that accepts the SSE connection but sends no
	// events, so the connection stays open and idle until we close it. This
	// reliably puts the goroutine into the backoff select after disconnect.
	connected := make(chan struct{}, 1)
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		connected <- struct{}{}
		<-r.Context().Done() // hold open until client disconnects
	}))
	defer tsrv.Close()

	c := NewClient(tsrv.URL)
	// Long constant backoff so the goroutine is guaranteed to be sleeping
	// when we call Unsubscribe.
	c.ReconnectStrategy = backoff.NewConstantBackOff(10 * time.Second)

	events := make(chan *Event)
	err := c.SubscribeChan("", events)
	require.Nil(t, err)

	// Wait for the server to confirm the connection is live.
	<-connected

	// Force the client into a reconnect cycle.
	tsrv.CloseClientConnections()

	// Give the goroutine time to enter the backoff sleep.
	time.Sleep(150 * time.Millisecond)

	// Unsubscribe must not block even though the goroutine is sleeping.
	done := make(chan struct{})
	go func() {
		c.Unsubscribe(events)
		close(done)
	}()

	select {
	case <-done:
		// good
	case <-time.After(time.Second):
		t.Fatal("Unsubscribe blocked while client was in reconnect backoff")
	}
}

func TestClientDoubleUnsubscribeNoDeadlock(t *testing.T) {
	// Two concurrent Unsubscribe calls must both return without deadlocking.
	setup(false)
	defer cleanup()

	c := NewClient(urlPath)

	events := make(chan *Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.Unsubscribe(events) }()
	go func() { defer wg.Done(); c.Unsubscribe(events) }()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		// good — both calls returned
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Unsubscribe calls deadlocked")
	}
}

type bodyCloseTracker struct {
	io.ReadCloser
	closed chan struct{}
}

func (b *bodyCloseTracker) Close() error {
	select {
	case b.closed <- struct{}{}:
	default:
	}
	return b.ReadCloser.Close()
}

// TestLastEventIDReset verifies that an empty id: field resets LastEventID
// to empty, per WHATWG SSE §9.2.6.
func TestLastEventIDReset(t *testing.T) {
	// First event sets id to 42, second event has empty id: which resets it.
	raw := "id: 42\ndata: first\n\nid:\ndata: second\n\n"

	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, raw)
		w.(http.Flusher).Flush()
	}))
	defer tsrv.Close()

	c := NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	var received []*Event
	var mu sync.Mutex
	done := make(chan struct{})

	go func() {
		c.SubscribeRaw(func(msg *Event) {
			mu.Lock()
			received = append(received, msg)
			if len(received) >= 2 {
				mu.Unlock()
				close(done)
				return
			}
			mu.Unlock()
		})
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for events")
	}

	// After the second event (id:\n), LastEventID should be empty.
	lastID, _ := c.LastEventID.Load().([]byte)
	assert.Empty(t, lastID, "LastEventID should be reset to empty by id: with empty value")
}

// TestLastEventIDPersistsWhenAbsent verifies that when no id: field is present,
// LastEventID remains unchanged from a previous event.
func TestLastEventIDPersistsWhenAbsent(t *testing.T) {
	// First event sets id to 42, second event has no id: field at all.
	raw := "id: 42\ndata: first\n\ndata: second\n\n"

	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, raw)
		w.(http.Flusher).Flush()
	}))
	defer tsrv.Close()

	c := NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	var received []*Event
	var mu sync.Mutex
	done := make(chan struct{})

	go func() {
		c.SubscribeRaw(func(msg *Event) {
			mu.Lock()
			received = append(received, msg)
			if len(received) >= 2 {
				mu.Unlock()
				close(done)
				return
			}
			mu.Unlock()
		})
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for events")
	}

	lastID, _ := c.LastEventID.Load().([]byte)
	assert.Equal(t, []byte("42"), lastID, "LastEventID should persist when id: field is absent")
}

// TestSubscribeFailsOnWrongContentType verifies that the client fails permanently
// when the server responds with a non-SSE Content-Type.
func TestSubscribeFailsOnWrongContentType(t *testing.T) {
	var requests int
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"error": "not sse"}`)
	}))
	defer tsrv.Close()

	c := NewClient(tsrv.URL)
	err := c.SubscribeRaw(func(msg *Event) {})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "content-type")
	assert.Equal(t, 1, requests, "client must not reconnect after wrong content-type")
}

// TestSubscribeAcceptsContentTypeWithParams verifies that text/event-stream
// with parameters (e.g. charset=utf-8) is accepted.
func TestSubscribeAcceptsContentTypeWithParams(t *testing.T) {
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: hello\n\n")
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
		// good — event received with parameterized content-type
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event with parameterized content-type")
	}
}

// TestRetryFieldUpdatesReconnectDelay verifies that a valid retry: field
// is applied to the reconnect delay, per WHATWG SSE §9.2.6.
func TestRetryFieldUpdatesReconnectDelay(t *testing.T) {
	var mu sync.Mutex
	var connectTimes []time.Time

	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connectTimes = append(connectTimes, time.Now())
		n := len(connectTimes)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if n == 1 {
			// First connection: send retry and data, then close.
			fmt.Fprint(w, "retry: 200\ndata: hello\n\n")
		} else {
			// Second connection: send data then close.
			fmt.Fprint(w, "data: world\n\n")
		}
		w.(http.Flusher).Flush()
	}))
	defer tsrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := NewClient(tsrv.URL)
	// Do NOT set ReconnectStrategy — let the default be used with retry override.

	var received int
	done := make(chan struct{})

	go func() {
		c.SubscribeRawWithContext(ctx, func(msg *Event) {
			received++
			if received >= 2 {
				close(done)
			}
		})
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for two events")
	}

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(connectTimes), 2)
	gap := connectTimes[1].Sub(connectTimes[0])
	assert.GreaterOrEqual(t, gap.Milliseconds(), int64(150),
		"reconnect delay should reflect retry: 200 field (got %v)", gap)
}

// TestRetryFieldIgnoresNonNumeric verifies that retry: with non-numeric value is ignored.
func TestRetryFieldIgnoresNonNumeric(t *testing.T) {
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	c.startReadLoop(reader)

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

// TestReadEventBufferOverflow verifies that when an SSE payload exceeds the
// scanner buffer, ReadEvent returns bufio.ErrTooLong rather than io.EOF.
// Regression test for sse-2e2.
func TestReadEventBufferOverflow(t *testing.T) {
	// Create a payload larger than the max buffer size.
	// 128 bytes of data with double-newline terminator, but maxBuffer=64.
	payload := bytes.Repeat([]byte("x"), 128)
	payload = append(payload, []byte("\n\n")...)

	reader := NewEventStreamReader(bytes.NewReader(payload), 64)
	_, err := reader.ReadEvent()

	require.Error(t, err)
	assert.NotEqual(t, io.EOF, err, "expected real scanner error, not io.EOF")
	assert.ErrorIs(t, err, bufio.ErrTooLong, "expected bufio.ErrTooLong")
}

// TestSubscribeWithContextCanceled verifies that SubscribeWithContext returns
// context.Canceled (not a backoff error) when the context is cancelled.
// Regression test for sse-1vw.
func TestSubscribeWithContextCanceled(t *testing.T) {
	setup(false)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	c := NewClient(urlPath)

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.SubscribeWithContext(ctx, "test", func(msg *Event) {})
	}()

	// Let the subscription establish, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled,
			"expected context.Canceled, got: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SubscribeWithContext to return")
	}
}

// TestIDFieldWithNullIgnored verifies that an id: field whose value contains
// U+0000 NULL is completely ignored, per WHATWG SSE §9.2.6.
// The event itself should still be dispatched (data is valid), but LastEventID
// must not be updated and IDPresent must be false.
func TestIDFieldWithNullIgnored(t *testing.T) {
	// id value contains a NULL byte — must be ignored.
	raw := "id: abc\x00def\ndata: hello\n\n"

	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, raw)
		w.(http.Flusher).Flush()
	}))
	defer tsrv.Close()

	c := NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	done := make(chan *Event, 1)
	go func() {
		c.SubscribeRaw(func(msg *Event) {
			done <- msg
		})
	}()

	select {
	case ev := <-done:
		// Event must be dispatched (data is valid).
		assert.Equal(t, []byte("hello"), ev.Data)
		// IDPresent must be false — the NULL-containing id was ignored.
		assert.False(t, ev.IDPresent, "IDPresent should be false when id contains NULL")
		// ID on the event should be empty (inherited from LastEventID which was never set).
		assert.Empty(t, ev.ID, "ID should be empty when id field contains NULL")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}

	// LastEventID must not have been updated.
	lastID, _ := c.LastEventID.Load().([]byte)
	assert.Nil(t, lastID, "LastEventID must not be set when id field contains NULL")
}

// TestEmptyDataWithIDNotDispatched verifies that an event containing only an
// id: field (no data:) is NOT dispatched to the handler, per WHATWG spec.
// The id should still be stored as LastEventID.
// Regression test for sse-pc8.
func TestEmptyDataWithIDNotDispatched(t *testing.T) {
	// Raw SSE stream: first event has id but no data, second has data.
	raw := "id: 42\n\ndata: hello\n\n"

	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		fmt.Fprint(w, raw)
		w.(http.Flusher).Flush()
	}))
	defer tsrv.Close()

	c := NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	var received []*Event
	var mu sync.Mutex
	done := make(chan struct{})

	go func() {
		c.SubscribeRaw(func(msg *Event) {
			mu.Lock()
			received = append(received, msg)
			mu.Unlock()
			if strings.TrimSpace(string(msg.Data)) == "hello" {
				close(done)
			}
		})
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for events")
	}

	mu.Lock()
	defer mu.Unlock()

	// Only the "hello" event should be dispatched; the id-only event must not.
	require.Len(t, received, 1, "expected 1 dispatched event, got %d", len(received))
	assert.Equal(t, []byte("hello"), received[0].Data)

	// LastEventID should still be set from the id-only event.
	lastID, _ := c.LastEventID.Load().([]byte)
	assert.Equal(t, []byte("42"), lastID, "LastEventID should be set even though event was not dispatched")
}
