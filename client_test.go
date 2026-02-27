/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// Black-box tests for client.go — package sse_test.
// TestTrimHeader (which needs the unexported trimHeader func and headerData
// var) is kept in client_internal_test.go (package sse) per the testpackage
// linter convention.
package sse_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sse "github.com/joshuafuller/sse/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	backoff "github.com/cenkalti/backoff/v4"
)

var urlPath string
var srv *sse.Server
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

func newServer() *sse.Server {
	srv = sse.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", srv.ServeHTTP)
	server = httptest.NewServer(mux)
	urlPath = server.URL + "/events"

	srv.CreateStream("test")

	return srv
}

func newServer401() *sse.Server {
	srv = sse.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	server = httptest.NewServer(mux)
	urlPath = server.URL + "/events"

	srv.CreateStream("test")

	return srv
}

func publishMsgs(s *sse.Server, empty bool, count int) {
	for a := 0; a < count; a++ {
		if empty {
			s.Publish("test", &sse.Event{Data: []byte("\n")})
		} else {
			s.Publish("test", &sse.Event{Data: []byte("ping")})
		}
		time.Sleep(time.Millisecond * 50)
	}
}

func publishMultilineMessages(s *sse.Server, count int) {
	for a := 0; a < count; a++ {
		s.Publish("test", &sse.Event{ID: []byte("123456"), Data: []byte(mldata)})
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

	c := sse.NewClient(urlPath)

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

	c := sse.NewClient(urlPath)

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

	c := sse.NewClient(urlPath)

	events := make(chan *sse.Event)
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

	c := sse.NewClient(urlPath)

	events := make(chan *sse.Event)
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
	// OnDisconnect fires when readLoop encounters a non-EOF, non-ErrUnexpectedEOF
	// error (sse-fyk: EOF and ErrUnexpectedEOF now trigger silent reconnects).
	//
	// Trigger bufio.ErrTooLong by sending an event whose data line exceeds
	// the client's max buffer size. This causes the scanner to return a real
	// error (not EOF), which fires disconnectcb.
	smallBuf := 64
	bigData := strings.Repeat("x", smallBuf*2) // guaranteed > buffer

	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Send a normal event first so we know the connection is up.
		fmt.Fprint(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		// Send an oversized event to trigger ErrTooLong.
		fmt.Fprintf(w, "data: %s\n\n", bigData)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer tsrv.Close()

	c := sse.NewClient(tsrv.URL, sse.ClientMaxBufferSize(smallBuf))
	// Stop after one attempt so we don't loop forever.
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	called := make(chan struct{}, 1)
	c.OnDisconnect(func(client *sse.Client) {
		select {
		case called <- struct{}{}:
		default:
		}
	})

	go c.SubscribeRaw(func(msg *sse.Event) {})

	select {
	case <-called:
		// good — disconnectcb fired on ErrTooLong
	case <-time.After(5 * time.Second):
		t.Fatal("OnDisconnect callback was not called within 5 seconds")
	}
}

func TestClientOnConnect(t *testing.T) {
	setup(false)
	defer cleanup()

	c := sse.NewClient(urlPath)

	called := make(chan struct{})
	c.OnConnect(func(client *sse.Client) {
		called <- struct{}{}
	})

	go c.Subscribe("test", func(msg *sse.Event) {})

	time.Sleep(time.Second)
	assert.Equal(t, struct{}{}, <-called)

	server.CloseClientConnections()
}

func TestClientChanReconnect(t *testing.T) {
	setup(false)
	defer cleanup()

	c := sse.NewClient(urlPath)

	events := make(chan *sse.Event)
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

	c := sse.NewClient(urlPath)

	events := make(chan *sse.Event)
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

	c := sse.NewClient(urlPath)

	events := make(chan *sse.Event)
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
	doneCh := make(chan *sse.Event)
	go func() {
		var e sse.Event
		c.Unsubscribe(events)
		doneCh <- &e
	}()
	_, merr := wait(doneCh, time.Millisecond*100)
	assert.Nil(t, merr)
}

func TestClientUnsubscribe401(t *testing.T) {
	srv = newServer401()
	defer cleanup()

	c := sse.NewClient(urlPath)

	// limit retries to 3
	c.ReconnectStrategy = backoff.WithMaxRetries(
		backoff.NewExponentialBackOff(),
		3,
	)

	err := c.SubscribeRaw(func(ev *sse.Event) {
		// this shouldn't run
		assert.False(t, true)
	})

	require.NotNil(t, err)
}

func TestClient204NoReconnect(t *testing.T) {
	// HTTP 204 must stop reconnection immediately — spec §9.2.3.
	var requests int
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer tsrv.Close()

	c := sse.NewClient(tsrv.URL)
	err := c.SubscribeRaw(func(ev *sse.Event) {})

	require.Error(t, err)
	assert.Equal(t, 1, requests, "client must not reconnect after 204")
}

func TestClientLargeData(t *testing.T) {
	srv = newServer()
	defer cleanup()

	c := sse.NewClient(urlPath, sse.ClientMaxBufferSize(1<<19))

	// limit retries to 3
	c.ReconnectStrategy = backoff.WithMaxRetries(
		backoff.NewExponentialBackOff(),
		3,
	)

	// allocate 128KB of data to send
	data := make([]byte, 1<<17)
	rand.Read(data)
	data = []byte(hex.EncodeToString(data))

	ec := make(chan *sse.Event, 1)

	srv.Publish("test", &sse.Event{Data: data})

	go func() {
		c.Subscribe("test", func(ev *sse.Event) {
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

	c := sse.NewClient(urlPath)

	events := make(chan *sse.Event)
	err := c.SubscribeChan("test", events)
	require.Nil(t, err)

	srv.Publish("test", &sse.Event{Comment: []byte("comment")})
	srv.Publish("test", &sse.Event{Data: []byte("test")})

	ev, err := waitEvent(events, time.Second*1)
	assert.Nil(t, err)
	assert.Equal(t, []byte("test"), ev.Data)

	c.Unsubscribe(events)
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

	c := sse.NewClient(tsrv.URL)

	for i := 0; i < numSubs; i++ {
		go func() {
			defer done.Done()
			once := sync.Once{}
			c.SubscribeWithContext(ctx, "", func(msg *sse.Event) {
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

	client := sse.NewClient(srv.URL)
	client.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)
	client.ResponseValidator = func(c *sse.Client, resp *http.Response) error {
		original := resp.Body
		resp.Body = &bodyCloseTracker{ReadCloser: original, closed: closeCalled}
		return fmt.Errorf("validator rejected response")
	}

	err := client.Subscribe("", func(msg *sse.Event) {})
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

	c := sse.NewClient(srv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 5)

	events := make(chan *sse.Event, 10)
	go c.SubscribeRawWithContext(ctx, func(msg *sse.Event) {
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

	c := sse.NewClient(tsrv.URL)
	// Long constant backoff so the goroutine is guaranteed to be sleeping
	// when we call Unsubscribe.
	c.ReconnectStrategy = backoff.NewConstantBackOff(10 * time.Second)

	events := make(chan *sse.Event)
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

	c := sse.NewClient(urlPath)

	events := make(chan *sse.Event)
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

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	var received []*sse.Event
	var mu sync.Mutex
	done := make(chan struct{})

	go func() {
		c.SubscribeRaw(func(msg *sse.Event) {
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

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	var received []*sse.Event
	var mu sync.Mutex
	done := make(chan struct{})

	go func() {
		c.SubscribeRaw(func(msg *sse.Event) {
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

	c := sse.NewClient(tsrv.URL)
	err := c.SubscribeRaw(func(msg *sse.Event) {})

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

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	done := make(chan struct{})
	go func() {
		c.SubscribeRaw(func(msg *sse.Event) {
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

	c := sse.NewClient(tsrv.URL)
	// Do NOT set ReconnectStrategy — let the default be used with retry override.

	var received int
	done := make(chan struct{})

	go func() {
		c.SubscribeRawWithContext(ctx, func(msg *sse.Event) {
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





// TestReadEventBufferOverflow verifies that when an SSE payload exceeds the
// scanner buffer, ReadEvent returns bufio.ErrTooLong rather than io.EOF.
// Regression test for sse-2e2.
func TestReadEventBufferOverflow(t *testing.T) {
	// Create a payload larger than the max buffer size.
	// 128 bytes of data with double-newline terminator, but maxBuffer=64.
	payload := bytes.Repeat([]byte("x"), 128)
	payload = append(payload, []byte("\n\n")...)

	reader := sse.NewEventStreamReader(bytes.NewReader(payload), 64)
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
	c := sse.NewClient(urlPath)

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.SubscribeWithContext(ctx, "test", func(msg *sse.Event) {})
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

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	done := make(chan *sse.Event, 1)
	go func() {
		c.SubscribeRaw(func(msg *sse.Event) {
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

// --- sse-71z: error wrapping ---

// TestRequestErrorsAreWrapped verifies that errors returned by request() are
// wrapped with fmt.Errorf("...: %w", err) so that the error message includes
// the wrapper context string.
func TestRequestErrorsAreWrapped(t *testing.T) {
	t.Run("bad URL error message contains wrapper prefix", func(t *testing.T) {
		// An invalid URL causes http.NewRequest to fail.
		// After wrapping the message should start with "create request:".
		c := sse.NewClient("://bad-url")
		c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

		err := c.SubscribeRaw(func(msg *sse.Event) {})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create request:",
			"expected error to contain 'create request:' wrapper prefix, got: %v", err)
	})

	t.Run("connection refused error message contains wrapper prefix", func(t *testing.T) {
		// Port 1 is almost always refused; Connection.Do fails.
		// After wrapping the message should start with "do request:".
		c := sse.NewClient("http://127.0.0.1:1/sse")
		c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

		err := c.SubscribeRaw(func(msg *sse.Event) {})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "do request:",
			"expected error to contain 'do request:' wrapper prefix, got: %v", err)
	})

	t.Run("errors.As still works through wrapper", func(t *testing.T) {
		// Wrapping with %w must not break errors.As traversal.
		c := sse.NewClient("://bad-url")
		c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

		var urlErr *url.Error
		err := c.SubscribeRaw(func(msg *sse.Event) {})
		require.Error(t, err)
		assert.True(t, errors.As(err, &urlErr),
			"expected errors.As to find *url.Error in wrapped error chain, got: %T: %v", err, err)
	})
}

// --- sse-c6q: backoff reset after successful connection ---

// TestBackoffResetAfterSuccessfulConnection verifies that after a successful
// SSE connection the backoff interval is reset to its minimum so that a
// subsequent reconnect does not start from an already-grown interval.
//
// Scenario:
//  1. First attempt fails (non-SSE response) → backoff advances to a large interval.
//  2. Second attempt succeeds with a real SSE connection then closes.
//     b.Reset() should fire here, resetting the interval back to InitialInterval.
//  3. Third attempt: measure how long after attempt 2 it happens.
//     Without Reset(), it would be ~Multiplier × InitialInterval (very long).
//     With Reset(), it should be ~InitialInterval (short).
func TestBackoffResetAfterSuccessfulConnection(t *testing.T) {
	var mu sync.Mutex
	var connectTimes []time.Time
	reqCount := 0

	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		n := reqCount
		connectTimes = append(connectTimes, time.Now())
		mu.Unlock()

		switch n {
		case 1:
			// First attempt: return a non-SSE, non-permanent error so backoff advances.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			// Second attempt: successful SSE connection, send one event then close.
			// Closing triggers an io.EOF reconnect, exercising b.Reset().
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: hello\n\n")
			w.(http.Flusher).Flush()
		default:
			// Third (and subsequent) attempts: successful SSE connection.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: world\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer tsrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := sse.NewClient(tsrv.URL)
	// Small InitialInterval, huge Multiplier: without Reset() the 3rd attempt
	// would be delayed by ~(10ms * 50) = 500ms; with Reset() it's ~10ms.
	eb := backoff.NewExponentialBackOff()
	eb.InitialInterval = 10 * time.Millisecond
	eb.Multiplier = 50
	eb.MaxInterval = 60 * time.Second
	eb.MaxElapsedTime = 0
	c.ReconnectStrategy = backoff.WithMaxRetries(eb, 10)

	received := make(chan string, 10)
	go func() {
		c.SubscribeRawWithContext(ctx, func(msg *sse.Event) {
			received <- string(msg.Data)
		})
	}()

	// Wait for two events (one from connection 2, one from connection 3).
	got := 0
	deadline := time.After(4 * time.Second)
	for got < 2 {
		select {
		case <-received:
			got++
		case <-deadline:
			t.Fatalf("timed out waiting for events; got %d so far", got)
		}
	}

	mu.Lock()
	times := append([]time.Time(nil), connectTimes...)
	mu.Unlock()

	require.GreaterOrEqual(t, len(times), 3, "expected at least 3 connections")
	// Gap between connection 2 (successful SSE) and connection 3 (post-drop reconnect).
	gap := times[2].Sub(times[1])
	// Without b.Reset(): gap ≈ InitialInterval * Multiplier^1 = 10ms * 50 = 500ms.
	// With b.Reset():    gap ≈ InitialInterval = 10ms.
	// Allow up to 400ms as a generous budget that still catches the regression.
	assert.Less(t, gap, 400*time.Millisecond,
		"reconnect gap after successful connection should be ~InitialInterval (%v), got %v — "+
			"possible missing b.Reset() after successful HTTP validation",
		eb.InitialInterval, gap)
}

// --- sse-a8c: context propagation into readLoop ---



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

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	var received []*sse.Event
	var mu sync.Mutex
	done := make(chan struct{})

	go func() {
		c.SubscribeRaw(func(msg *sse.Event) {
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

// --- sse-gky: sse.StreamError type wraps non-200 status codes ---

// TestStreamErrorWrapsNon200 verifies that when the server returns a non-200
// status, the error is wrapped as a *sse.StreamError so callers can use errors.As.
func TestStreamErrorWrapsNon200(t *testing.T) {
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("service unavailable"))
	}))
	defer tsrv.Close()

	c := sse.NewClient(tsrv.URL)
	// Stop after one attempt so the test finishes quickly.
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	err := c.SubscribeRaw(func(ev *sse.Event) {})
	require.Error(t, err)

	var se *sse.StreamError
	require.True(t, errors.As(err, &se), "expected *sse.StreamError in error chain, got: %T: %v", err, err)
	assert.Equal(t, http.StatusServiceUnavailable, se.StatusCode)
	assert.Contains(t, string(se.Body), "service unavailable")
	assert.Contains(t, err.Error(), "could not connect to stream")
}

// TestStreamErrorBodyCappedAt512 verifies that sse.StreamError.Body captures at
// most 512 bytes of the response body.
func TestStreamErrorBodyCappedAt512(t *testing.T) {
	bigBody := bytes.Repeat([]byte("x"), 1024)
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write(bigBody)
	}))
	defer tsrv.Close()

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	err := c.SubscribeRaw(func(ev *sse.Event) {})
	require.Error(t, err)

	var se *sse.StreamError
	require.True(t, errors.As(err, &se))
	assert.LessOrEqual(t, len(se.Body), 512, "sse.StreamError.Body must be capped at 512 bytes")
}

// TestStreamErrorViaChanSubscribe verifies that SubscribeChanWithContext also
// returns a *sse.StreamError-wrapped error on non-200.
func TestStreamErrorViaChanSubscribe(t *testing.T) {
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden"))
	}))
	defer tsrv.Close()

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	ch := make(chan *sse.Event)
	err := c.SubscribeChan("", ch)
	require.Error(t, err)

	var se *sse.StreamError
	require.True(t, errors.As(err, &se), "expected *sse.StreamError, got: %T: %v", err, err)
	assert.Equal(t, http.StatusForbidden, se.StatusCode)
}

// --- sse-6v2: OnConnect fires immediately after HTTP handshake ---

// TestOnConnectFiresWithoutEvents verifies that the OnConnect callback fires
// even when the server sends no events — it must fire as soon as the HTTP
// 200+text/event-stream response is received, not when the first event arrives.
func TestOnConnectFiresWithoutEvents(t *testing.T) {
	// Server holds the connection open but never sends any events.
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Hold open until client disconnects.
		<-r.Context().Done()
	}))
	defer tsrv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	c := sse.NewClient(tsrv.URL)
	connected := make(chan struct{}, 1)
	c.OnConnect(func(cl *sse.Client) {
		select {
		case connected <- struct{}{}:
		default:
		}
	})

	go c.SubscribeRawWithContext(ctx, func(msg *sse.Event) {})

	// OnConnect must fire within 500ms even though the server sends no events.
	select {
	case <-connected:
		// good — callback fired without needing an event
	case <-time.After(500 * time.Millisecond):
		t.Fatal("OnConnect did not fire within 500ms; server sent no events")
	}

	cancel()
}

// TestOnConnectFiresBeforeFirstEvent verifies that the OnConnect callback fires
// before any events are dispatched to the handler.
func TestOnConnectFiresBeforeFirstEvent(t *testing.T) {
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Small delay then send an event.
		time.Sleep(100 * time.Millisecond)
		fmt.Fprint(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer tsrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := sse.NewClient(tsrv.URL)

	var order []string
	var orderMu sync.Mutex
	addOrder := func(s string) {
		orderMu.Lock()
		order = append(order, s)
		orderMu.Unlock()
	}

	eventReceived := make(chan struct{}, 1)
	c.OnConnect(func(cl *sse.Client) {
		addOrder("connect")
	})

	go c.SubscribeRawWithContext(ctx, func(msg *sse.Event) {
		addOrder("event")
		select {
		case eventReceived <- struct{}{}:
		default:
		}
	})

	select {
	case <-eventReceived:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}

	orderMu.Lock()
	got := append([]string(nil), order...)
	orderMu.Unlock()

	require.GreaterOrEqual(t, len(got), 2)
	assert.Equal(t, "connect", got[0], "OnConnect must fire before the first event handler call; order: %v", got)
	assert.Equal(t, "event", got[1])
}

// --- sse-fyk: io.ErrUnexpectedEOF treated like io.EOF (silent reconnect) ---


// --- sse-x43: non-GET methods and request body ---

// TestClientDefaultMethodIsGET verifies that a Client with no Method set sends
// a GET request (preserving existing behaviour).
func TestClientDefaultMethodIsGET(t *testing.T) {
	var gotMethod string
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Send one event then close.
		_, _ = fmt.Fprint(w, "data: hello\n\n")
	}))
	defer tsrv.Close()

	c := sse.NewClient(tsrv.URL)
	// Use a strategy that stops immediately after the first attempt.
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 100 * time.Millisecond
	c.ReconnectStrategy = b

	_ = c.SubscribeRaw(func(msg *sse.Event) {})

	assert.Equal(t, http.MethodGet, gotMethod, "default method must be GET")
}

// TestClientPOSTWithBody verifies that setting Method and Body causes the
// client to send a POST with the provided body.
func TestClientPOSTWithBody(t *testing.T) {
	var gotMethod string
	var gotBody string
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: hello\n\n")
	}))
	defer tsrv.Close()

	const payload = `{"stream":"test"}`
	c := sse.NewClient(tsrv.URL)
	c.Method = http.MethodPost
	c.Body = func() io.Reader { return strings.NewReader(payload) }

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 100 * time.Millisecond
	c.ReconnectStrategy = b

	_ = c.SubscribeRaw(func(msg *sse.Event) {})

	assert.Equal(t, http.MethodPost, gotMethod, "method must be POST")
	assert.Equal(t, payload, gotBody, "request body must match payload")
}

// --- sse-frd: WP-004 — CRLF as single line ending ---









// --- sse-482: CL-004 — Sanitize Last-Event-ID header value ---

// TestLastEventIDHeaderSanitized verifies that forbidden characters (NULL,
// LF, CR) are stripped from the Last-Event-ID header before it is sent,
// per WHATWG SSE §9.2.1 / WHATWG Fetch §2.2 (header value must not contain
// U+0000, U+000A, or U+000D).
func TestLastEventIDHeaderSanitized(t *testing.T) {
	t.Parallel()

	var capturedID string
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
	}))
	defer tsrv.Close()

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	// Store a LastEventID that contains all three forbidden characters.
	// After sanitization the header value should be "abcdef" (forbidden chars dropped).
	c.LastEventID.Store([]byte("abc\x00\ndef\r"))

	done := make(chan struct{})
	go func() {
		c.SubscribeRaw(func(msg *sse.Event) {
			close(done)
		})
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}

	// The header must not contain any forbidden characters.
	assert.NotContains(t, capturedID, "\x00", "Last-Event-ID header must not contain NULL")
	assert.NotContains(t, capturedID, "\n", "Last-Event-ID header must not contain LF")
	assert.NotContains(t, capturedID, "\r", "Last-Event-ID header must not contain CR")
	// The clean portion of the ID must be preserved.
	assert.Equal(t, "abcdef", capturedID, "Last-Event-ID must contain only the clean portion of the ID")
}

// TestLastEventIDHeaderOmittedWhenAllForbidden verifies that if the entire
// Last-Event-ID value consists of forbidden characters, the header is omitted
// entirely (not sent as an empty header).
func TestLastEventIDHeaderOmittedWhenAllForbidden(t *testing.T) {
	t.Parallel()

	var headerPresent bool
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http canonicalises "Last-Event-ID" to "Last-Event-Id".
		_, headerPresent = r.Header["Last-Event-Id"]
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
	}))
	defer tsrv.Close()

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 0)

	// All characters are forbidden — the sanitized value will be empty, so the
	// header should not be sent at all.
	c.LastEventID.Store([]byte("\x00\n\r"))

	done := make(chan struct{})
	go func() {
		c.SubscribeRaw(func(msg *sse.Event) {
			close(done)
		})
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
	}

	assert.False(t, headerPresent,
		"Last-Event-ID header must be omitted when all characters are forbidden")
}

// TestClientBodyFactoryCalledOnReconnect verifies that the Body factory is
// called once per connection attempt (including reconnects), not just once.
// The server returns 503 twice then 200 with a single event on the third
// attempt; it returns 204 for all subsequent requests so the backoff loop
// terminates cleanly. We check that callCount is exactly 3 after the 200.
func TestClientBodyFactoryCalledOnReconnect(t *testing.T) {
	var callCount int32
	var reqCount int32

	// eventReceived is closed when the handler receives the first event.
	eventReceived := make(chan struct{})
	var eventOnce sync.Once

	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&reqCount, 1)
		// Drain the body so the connection is not stalled.
		_, _ = io.ReadAll(r.Body)
		if n < 3 {
			http.Error(w, "not yet", http.StatusServiceUnavailable)
			return
		}
		if n == 3 {
			// Third attempt: serve one SSE event then close.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "data: hello\n\n")
			return
		}
		// Fourth+ attempts: return 204 to permanently stop reconnecting.
		w.WriteHeader(http.StatusNoContent)
	}))
	defer tsrv.Close()

	c := sse.NewClient(tsrv.URL)
	c.Method = http.MethodPost
	c.Body = func() io.Reader {
		atomic.AddInt32(&callCount, 1)
		return strings.NewReader(`{"stream":"test"}`)
	}

	// Use a fast backoff so the test doesn't take long.
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 5 * time.Millisecond
	b.MaxInterval = 10 * time.Millisecond
	b.MaxElapsedTime = 5 * time.Second
	c.ReconnectStrategy = b

	_ = c.SubscribeRaw(func(msg *sse.Event) {
		eventOnce.Do(func() { close(eventReceived) })
	})

	// Wait for the event to be received before checking callCount.
	select {
	case <-eventReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event to be received")
	}

	got := atomic.LoadInt32(&callCount)
	assert.GreaterOrEqual(t, got, int32(3), "Body factory must be called at least 3 times (once per attempt); got %d", got)
}

// --- sse-tem: SubscribeChanRaw and SubscribeChanRawWithContext smoke tests ---

// newRawSSEServer returns a test HTTP server that immediately streams a single
// SSE event with the given data and then holds the connection open until the
// client disconnects. The connected channel receives a value once the handler
// is active.
func newRawSSEServer(data string, connected chan struct{}) *httptest.Server {
	once := sync.Once{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
		once.Do(func() { connected <- struct{}{} })
		<-r.Context().Done()
	}))
}

// TestClientSubscribeChanRaw verifies that SubscribeChanRaw is a transparent
// wrapper around SubscribeChan("", ch): events sent by the server without a
// stream query parameter are delivered to the channel.
func TestClientSubscribeChanRaw(t *testing.T) {
	connected := make(chan struct{}, 1)
	tsrv := newRawSSEServer("ping", connected)
	defer tsrv.Close()

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

	events := make(chan *sse.Event)
	err := c.SubscribeChanRaw(events)
	require.Nil(t, err)

	<-connected

	msg, merr := wait(events, time.Second)
	require.Nil(t, merr)
	assert.Equal(t, []byte("ping"), msg)

	c.Unsubscribe(events)
}

// TestClientSubscribeChanRawWithContext verifies that
// SubscribeChanRawWithContext is a transparent wrapper around
// SubscribeChanWithContext(ctx, "", ch): events are delivered and cancelling
// the context stops delivery cleanly.
func TestClientSubscribeChanRawWithContext(t *testing.T) {
	connected := make(chan struct{}, 1)
	tsrv := newRawSSEServer("ping", connected)
	defer tsrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := sse.NewClient(tsrv.URL)
	c.ReconnectStrategy = backoff.NewConstantBackOff(50 * time.Millisecond)

	events := make(chan *sse.Event)
	err := c.SubscribeChanRawWithContext(ctx, events)
	require.Nil(t, err)

	<-connected

	msg, merr := wait(events, time.Second)
	require.Nil(t, merr)
	assert.Equal(t, []byte("ping"), msg)

	cancel()
	c.Unsubscribe(events)
}
