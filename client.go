/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
)

var (
	headerID    = []byte("id:")
	headerData  = []byte("data:")
	headerEvent = []byte("event:")
	headerRetry = []byte("retry:")
)

// ErrEmptyEventMessage is returned by the event parser when a raw event block
// contains no bytes. Callers can detect this with errors.Is.
var ErrEmptyEventMessage = errors.New("event message was empty")

// StreamError is returned when the server responds with a non-200 HTTP status.
// Callers can inspect the status code and raw body via errors.As.
type StreamError struct {
	StatusCode int
	Body       []byte // first 512 bytes of the response body
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("could not connect to stream: %s", http.StatusText(e.StatusCode))
}

// ClientMaxBufferSize sets the maximum buffer size for reading SSE events.
func ClientMaxBufferSize(s int) func(c *Client) {
	return func(c *Client) {
		c.maxBufferSize = s
	}
}

// ConnCallback defines a function to be called on a particular connection event.
type ConnCallback func(c *Client)

// ResponseValidator validates a response.
type ResponseValidator func(c *Client, resp *http.Response) error

// Client handles an incoming server stream.
type Client struct {
	Retry time.Time

	// ReconnectStrategy controls how the client retries failed or dropped
	// connections. If nil, a default backoff.NewExponentialBackOff() is used,
	// which retries with exponential delay but stops permanently after 15
	// minutes (MaxElapsedTime = 15 * time.Minute).
	//
	// To retry indefinitely, set an explicit strategy with MaxElapsedTime = 0:
	//
	//	b := backoff.NewExponentialBackOff()
	//	b.MaxElapsedTime = 0 // 0 means no time limit
	//	client.ReconnectStrategy = b
	ReconnectStrategy backoff.BackOff

	// Method is the HTTP method used for SSE requests. Defaults to GET when
	// empty.
	Method string

	// Body is an optional factory function called before each request attempt
	// (including reconnects) to produce a fresh io.Reader for the request body.
	// A factory is used rather than a plain io.Reader so that reconnects always
	// read the body from the start. If nil, no body is sent.
	Body func() io.Reader

	disconnectcb      ConnCallback
	connectedcb       ConnCallback
	subscribed        map[chan *Event]chan struct{}
	Headers           map[string]string
	ReconnectNotify   backoff.Notify
	ResponseValidator ResponseValidator
	Connection        *http.Client
	URL               string
	LastEventID       atomic.Value // []byte
	maxBufferSize     int
	mu                sync.Mutex
	retryDelay        time.Duration
	EncodingBase64    bool
	connected         atomic.Bool // protected by atomic; use Connected() / setConnected()
}

// NewClient creates a new client.
func NewClient(url string, opts ...func(c *Client)) *Client {
	c := &Client{
		URL:           url,
		Connection:    &http.Client{},
		Headers:       make(map[string]string),
		subscribed:    make(map[chan *Event]chan struct{}),
		maxBufferSize: 1 << 16,
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// Connected reports whether the client is currently connected to the SSE
// stream.  It is safe to call from any goroutine.
func (c *Client) Connected() bool {
	return c.connected.Load()
}

// Subscribe connects to a named SSE stream and calls handler for each event.
// It blocks until the server stops retrying (see ReconnectStrategy).
// By default, reconnection uses backoff.NewExponentialBackOff() which stops
// after 15 minutes; set ReconnectStrategy with MaxElapsedTime = 0 to retry
// indefinitely.
func (c *Client) Subscribe(stream string, handler func(msg *Event)) error {
	return c.SubscribeWithContext(context.Background(), stream, handler)
}

// SubscribeWithContext connects to a named SSE stream and calls handler for
// each received event. It blocks until ctx is cancelled, the server sends HTTP
// 204, or the reconnect strategy gives up.
//
// Reconnect behaviour: on any connection error or EOF the client waits
// according to ReconnectStrategy before reconnecting. If ReconnectStrategy is
// nil a default backoff.NewExponentialBackOff() is used, which stops after 15
// minutes. Cancelling ctx always terminates the subscription immediately.
func (c *Client) SubscribeWithContext(ctx context.Context, stream string, handler func(msg *Event)) error {
	// Apply user specified reconnection strategy or default to standard NewExponentialBackOff() reconnection method.
	// Wrap with context so cancellation stops the retry loop.
	var b backoff.BackOff
	if c.ReconnectStrategy != nil {
		b = backoff.WithContext(c.ReconnectStrategy, ctx)
	} else {
		eb := backoff.NewExponentialBackOff()
		c.mu.Lock()
		if c.retryDelay > 0 {
			eb.InitialInterval = c.retryDelay
		}
		c.mu.Unlock()
		b = backoff.WithContext(eb, ctx)
	}

	operation := func() error {
		resp, err := c.request(ctx, stream)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		if validator := c.ResponseValidator; validator != nil {
			err = validator(c, resp)
			if err != nil {
				return err
			}
		} else {
			if resp.StatusCode == http.StatusNoContent {
				return backoff.Permanent(fmt.Errorf("server returned 204 No Content: reconnection disabled"))
			}
			if resp.StatusCode != http.StatusOK {
				body := make([]byte, 512)
				n, _ := io.ReadFull(resp.Body, body)
				return fmt.Errorf("%w", &StreamError{StatusCode: resp.StatusCode, Body: body[:n]})
			}
			ct := resp.Header.Get("Content-Type")
			mediaType, _, _ := mime.ParseMediaType(ct)
			if mediaType != "text/event-stream" {
				return backoff.Permanent(fmt.Errorf("invalid content-type %q: expected text/event-stream", ct))
			}
		}

		if c.connected.CompareAndSwap(false, true) && c.connectedcb != nil {
			c.connectedcb(c)
		}

		b.Reset()
		reader := NewEventStreamReader(resp.Body, c.maxBufferSize)
		eventChan, errorChan := c.startReadLoop(ctx, reader)

		for {
			select {
			case err = <-errorChan:
				return err
			case msg := <-eventChan:
				handler(msg)
			}
		}
	}

	return backoff.RetryNotify(operation, b, c.ReconnectNotify)
}

// SubscribeChan sends all events to the provided channel.
func (c *Client) SubscribeChan(stream string, ch chan *Event) error {
	return c.SubscribeChanWithContext(context.Background(), stream, ch)
}

// SubscribeChanWithContext sends all events to the provided channel with context.
func (c *Client) SubscribeChanWithContext(ctx context.Context, stream string, ch chan *Event) error {
	var connected bool
	errch := make(chan error)
	c.mu.Lock()
	c.subscribed[ch] = make(chan struct{}, 1)
	// Capture the quit channel before launching the goroutine so the goroutine
	// never reads c.subscribed[ch] (a map lookup) without holding c.mu. This
	// eliminates the data race between the goroutine's map reads (lines below)
	// and concurrent map writes in other SubscribeChanWithContext calls.
	quitCh := c.subscribed[ch]
	c.mu.Unlock()

	go func() {
		defer c.cleanup(ch)
		// Apply user specified reconnection strategy or default to standard NewExponentialBackOff() reconnection method.
		// Wrap with context so cancellation stops the retry loop.
		var b backoff.BackOff
		if c.ReconnectStrategy != nil {
			b = backoff.WithContext(c.ReconnectStrategy, ctx)
		} else {
			eb := backoff.NewExponentialBackOff()
			c.mu.Lock()
			if c.retryDelay > 0 {
				eb.InitialInterval = c.retryDelay
			}
			c.mu.Unlock()
			b = backoff.WithContext(eb, ctx)
		}

		operation := func() error {
			resp, err := c.request(ctx, stream)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()

			if validator := c.ResponseValidator; validator != nil {
				err = validator(c, resp)
				if err != nil {
					return err
				}
			} else {
				if resp.StatusCode == http.StatusNoContent {
					return backoff.Permanent(fmt.Errorf("server returned 204 No Content: reconnection disabled"))
				}
				if resp.StatusCode != http.StatusOK {
					body := make([]byte, 512)
					n, _ := io.ReadFull(resp.Body, body)
					return fmt.Errorf("%w", &StreamError{StatusCode: resp.StatusCode, Body: body[:n]})
				}
				ct := resp.Header.Get("Content-Type")
				mediaType, _, _ := mime.ParseMediaType(ct)
				if mediaType != "text/event-stream" {
					return backoff.Permanent(fmt.Errorf("invalid content-type %q: expected text/event-stream", ct))
				}
			}

			if c.connected.CompareAndSwap(false, true) && c.connectedcb != nil {
				c.connectedcb(c)
			}

			if !connected {
				// Notify connect
				errch <- nil
				connected = true
			}

			b.Reset()
			reader := NewEventStreamReader(resp.Body, c.maxBufferSize)
			eventChan, errorChan := c.startReadLoop(ctx, reader)

			for {
				var msg *Event
				// Wait for message to arrive or exit
				select {
				case <-quitCh:
					return nil
				case err = <-errorChan:
					return err
				case msg = <-eventChan:
				}

				// Wait for message to be sent or exit
				if msg != nil {
					select {
					case <-quitCh:
						return nil
					case ch <- msg:
						// message sent
					}
				}
			}
		}

		err := backoff.RetryNotify(operation, b, c.ReconnectNotify)

		// channel closed once connected
		if err != nil && !connected {
			errch <- err
		}
	}()
	err := <-errch
	close(errch)
	return err
}

func (c *Client) startReadLoop(ctx context.Context, reader *EventStreamReader) (chan *Event, chan error) {
	outCh := make(chan *Event)
	erChan := make(chan error, 1)
	go c.readLoop(ctx, reader, outCh, erChan)
	return outCh, erChan
}

func (c *Client) readLoop(ctx context.Context, reader *EventStreamReader, outCh chan *Event, erChan chan error) {
	for {
		// Check for context cancellation before each read attempt.
		select {
		case <-ctx.Done():
			erChan <- ctx.Err()
			return
		default:
		}

		// Read each new line and process the type of event
		event, err := reader.ReadEvent()
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				// EOF and ErrUnexpectedEOF both trigger a silent reconnect via
				// backoff. Do not fire disconnectcb — a transient keepalive
				// timeout is not a real disconnect.
				erChan <- err
				return
			}
			// run user specified disconnect function
			if c.disconnectcb != nil {
				c.connected.Store(false)
				c.disconnectcb(c)
			}
			erChan <- err
			return
		}

		// If we get an error, ignore it.
		var msg *Event
		if msg, err = c.processEvent(event); err == nil {
			if msg.IDPresent {
				// id: field was explicitly present; store the value (may be empty)
				c.LastEventID.Store(msg.ID)
			} else {
				// No id: field; inherit previous LastEventID for the event struct only
				msg.ID, _ = c.LastEventID.Load().([]byte)
			}

			// Apply retry: field if it contains only ASCII digits.
			if len(msg.Retry) > 0 {
				if n, err := strconv.ParseInt(string(msg.Retry), 10, 64); err == nil {
					c.mu.Lock()
					c.retryDelay = time.Duration(n) * time.Millisecond
					c.mu.Unlock()
				}
			}

			// Per WHATWG spec: if the data buffer is empty, do not dispatch
			// the event. ID-only events update LastEventID above but must
			// not be sent downstream.
			if len(msg.Data) > 0 || len(msg.Event) > 0 || len(msg.Retry) > 0 {
				outCh <- msg
			}
		}

		// Check for context cancellation after each successful event dispatch.
		select {
		case <-ctx.Done():
			erChan <- ctx.Err()
			return
		default:
		}
	}
}

// SubscribeRaw to an sse endpoint.
func (c *Client) SubscribeRaw(handler func(msg *Event)) error {
	return c.Subscribe("", handler)
}

// SubscribeRawWithContext to an sse endpoint with context.
func (c *Client) SubscribeRawWithContext(ctx context.Context, handler func(msg *Event)) error {
	return c.SubscribeWithContext(ctx, "", handler)
}

// SubscribeChanRaw sends all events to the provided channel.
func (c *Client) SubscribeChanRaw(ch chan *Event) error {
	return c.SubscribeChan("", ch)
}

// SubscribeChanRawWithContext sends all events to the provided channel with context.
func (c *Client) SubscribeChanRawWithContext(ctx context.Context, ch chan *Event) error {
	return c.SubscribeChanWithContext(ctx, "", ch)
}

// Unsubscribe unsubscribes a channel.
func (c *Client) Unsubscribe(ch chan *Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.subscribed[ch] != nil {
		select {
		case c.subscribed[ch] <- struct{}{}:
		default:
			// already signalled; goroutine will pick it up when it next enters select
		}
	}
}

// OnDisconnect specifies the function to run when the connection disconnects.
func (c *Client) OnDisconnect(fn ConnCallback) {
	c.disconnectcb = fn
}

// OnConnect specifies the function to run when the connection is successful.
func (c *Client) OnConnect(fn ConnCallback) {
	c.connectedcb = fn
}

func (c *Client) request(ctx context.Context, stream string) (*http.Response, error) {
	method := c.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if c.Body != nil {
		body = c.Body()
	}
	req, err := http.NewRequest(method, c.URL, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req = req.WithContext(ctx)

	// Setup request, specify stream to connect to
	if stream != "" {
		query := req.URL.Query()
		query.Add("stream", stream)
		req.URL.RawQuery = query.Encode()
	}

	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "keep-alive")

	lastID, exists := c.LastEventID.Load().([]byte)
	if exists && lastID != nil {
		// Per WHATWG SSE §9.2.1 / WHATWG Fetch §2.2: the Last-Event-ID header
		// value MUST NOT contain U+0000 NULL, U+000A LF, or U+000D CR.  Strip
		// any such characters before setting the header; if nothing survives,
		// omit the header entirely (net/http rejects headers with these bytes).
		safe := bytes.Map(func(r rune) rune {
			if r == 0 || r == '\n' || r == '\r' {
				return -1 // drop
			}
			return r
		}, lastID)
		if len(safe) > 0 {
			req.Header.Set("Last-Event-ID", string(safe))
		}
	}

	// Add user specified headers
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}

	resp, err := c.Connection.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	return resp, nil
}

// splitLines splits SSE event bytes into individual field lines following the
// WHATWG SSE §9.2.6 line-ending rules:
//   - CR immediately followed by LF (\r\n) is a SINGLE line ending (not two).
//   - Bare LF (\n) is a single line ending.
//   - Bare CR (\r) is a single line ending.
//
// Unlike bytes.FieldsFunc (which collapses consecutive separator characters),
// splitLines preserves empty lines and advances exactly the right number of bytes
// per line ending, making the spec compliance explicit and unambiguous.
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	for len(data) > 0 {
		idx := bytes.IndexAny(data, "\r\n")
		if idx < 0 {
			lines = append(lines, data)
			break
		}
		lines = append(lines, data[:idx])
		if data[idx] == '\r' && idx+1 < len(data) && data[idx+1] == '\n' {
			data = data[idx+2:] // consume CRLF as one line ending
		} else {
			data = data[idx+1:] // consume bare CR or bare LF as one line ending
		}
	}
	return lines
}

func (c *Client) processEvent(msg []byte) (event *Event, err error) {
	var e Event

	if len(msg) < 1 {
		return nil, ErrEmptyEventMessage
	}

	// Split the event block into individual field lines.
	// splitLines treats \r\n as a SINGLE line ending per WHATWG SSE §9.2.6 (WP-004),
	// unlike bytes.FieldsFunc which collapses consecutive \r and \n characters.
	for _, line := range splitLines(msg) {
		switch {
		case bytes.HasPrefix(line, headerID):
			val := trimHeader(len(headerID), line)
			if !bytes.ContainsRune(val, 0) { // ignore if contains NULL
				e.ID = append([]byte(nil), val...)
				e.IDPresent = true
			}
		case bytes.HasPrefix(line, headerData):
			// The spec allows for multiple data fields per event, concatenated them with "\n".
			e.Data = append(e.Data[:], append(trimHeader(len(headerData), line), byte('\n'))...)
		// The spec says that a line that simply contains the string "data" should be treated as a data field with an empty body.
		case bytes.Equal(line, bytes.TrimSuffix(headerData, []byte(":"))):
			e.Data = append(e.Data, byte('\n'))
		case bytes.HasPrefix(line, headerEvent):
			e.Event = append([]byte(nil), trimHeader(len(headerEvent), line)...)
		case bytes.HasPrefix(line, headerRetry):
			e.Retry = append([]byte(nil), trimHeader(len(headerRetry), line)...)
		default:
			// Ignore any garbage that doesn't match what we're looking for.
		}
	}

	// Trim the last "\n" per the spec.
	e.Data = bytes.TrimSuffix(e.Data, []byte("\n"))

	if c.EncodingBase64 {
		buf := make([]byte, base64.StdEncoding.DecodedLen(len(e.Data)))

		n, decodeErr := base64.StdEncoding.Decode(buf, e.Data)
		if decodeErr != nil {
			err = fmt.Errorf("failed to decode event message: %w", decodeErr)
		}
		e.Data = buf[:n]
	}
	return &e, err
}

func (c *Client) cleanup(ch chan *Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.subscribed[ch] != nil {
		close(c.subscribed[ch])
		delete(c.subscribed, ch)
	}
}

func trimHeader(size int, data []byte) []byte {
	if len(data) < size {
		return data
	}

	data = data[size:]
	// Remove optional leading whitespace
	if len(data) > 0 && data[0] == 32 {
		data = data[1:]
	}
	// Remove trailing new line
	if len(data) > 0 && data[len(data)-1] == 10 {
		data = data[:len(data)-1]
	}
	return data
}
