/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import (
	"net/url"
	"sync"
	"sync/atomic"
)

// Stream manages the lifecycle of a named event channel: it runs a dispatch
// goroutine, tracks subscribers, and optionally replays the event log to
// newly connected clients.
type Stream struct {
	ID              string
	event           chan *Event
	quit            chan struct{}
	quitOnce        sync.Once
	register        chan *Subscriber
	deregister      chan *Subscriber
	subscribers     []*Subscriber
	Eventlog        EventLog
	subscriberCount int32
	// Enables replaying of eventlog to newly added subscribers
	AutoReplay   bool
	isAutoStream bool

	// MaxSubscribers caps the number of concurrent subscribers for this
	// stream. When MaxSubscribers > 0 and the current subscriber count is
	// at or above the cap, addSubscriber returns (nil, false) and the
	// caller should respond with HTTP 429 Too Many Requests.
	// A value of 0 (the default) means unlimited.
	MaxSubscribers int

	// Specifies the function to run when client subscribe or un-subscribe.
	// These are set from the server-level callbacks via newStream.
	OnSubscribe   func(streamID string, sub *Subscriber)
	OnUnsubscribe func(streamID string, sub *Subscriber)

	// mu protects the per-stream callback fields below.
	mu sync.RWMutex

	// Per-stream callbacks that fire in addition to the server-level ones.
	// Use SetOnSubscribe / SetOnUnsubscribe to set these safely.
	streamOnSubscribe   func(streamID string, sub *Subscriber)
	streamOnUnsubscribe func(streamID string, sub *Subscriber)
}

// SetOnSubscribe sets a per-stream subscribe callback that fires in addition
// to the server-level OnSubscribe callback. It is safe to call concurrently.
func (str *Stream) SetOnSubscribe(fn func(streamID string, sub *Subscriber)) {
	str.mu.Lock()
	str.streamOnSubscribe = fn
	str.mu.Unlock()
}

// SetOnUnsubscribe sets a per-stream unsubscribe callback that fires in
// addition to the server-level OnUnsubscribe callback. It is safe to call
// concurrently.
func (str *Stream) SetOnUnsubscribe(fn func(streamID string, sub *Subscriber)) {
	str.mu.Lock()
	str.streamOnUnsubscribe = fn
	str.mu.Unlock()
}

// newStream returns a new stream.
func newStream(id string, buffSize int, replay, isAutoStream bool, onSubscribe, onUnsubscribe func(string, *Subscriber)) *Stream {
	return &Stream{
		ID:            id,
		AutoReplay:    replay,
		subscribers:   make([]*Subscriber, 0),
		isAutoStream:  isAutoStream,
		register:      make(chan *Subscriber),
		deregister:    make(chan *Subscriber),
		event:         make(chan *Event, buffSize),
		quit:          make(chan struct{}),
		Eventlog:      EventLog{},
		OnSubscribe:   onSubscribe,
		OnUnsubscribe: onUnsubscribe,
	}
}

func (str *Stream) run() {
	go func(str *Stream) {
		for {
			select {
			// Add new subscriber
			case subscriber := <-str.register:
				str.subscribers = append(str.subscribers, subscriber)
				if str.AutoReplay {
					str.Eventlog.Replay(subscriber)
				}

			// Remove closed subscriber
			case subscriber := <-str.deregister:
				i := str.getSubIndex(subscriber)
				if i != -1 {
					str.removeSubscriber(i)
				}

				if str.OnUnsubscribe != nil {
					go str.OnUnsubscribe(str.ID, subscriber)
				}

				str.mu.RLock()
				perStreamUnsub := str.streamOnUnsubscribe
				str.mu.RUnlock()
				if perStreamUnsub != nil {
					go perStreamUnsub(str.ID, subscriber)
				}

			// Publish event to subscribers
			case event := <-str.event:
				if str.AutoReplay {
					str.Eventlog.Add(event)
				}
				for i := range str.subscribers {
					select {
					case str.subscribers[i].connection <- event:
					default:
						// Drop event for slow subscriber to avoid blocking the stream
					}
				}

			// Shutdown if the server closes
			case <-str.quit:
				// remove connections
				str.removeAllSubscribers()
				return
			}
		}
	}(str)
}

func (str *Stream) close() {
	str.quitOnce.Do(func() {
		close(str.quit)
	})
}

func (str *Stream) getSubIndex(sub *Subscriber) int {
	for i := range str.subscribers {
		if str.subscribers[i] == sub {
			return i
		}
	}
	return -1
}

// addSubscriber will create a new subscriber on a stream.
// It returns (subscriber, true) on success, or (nil, false) when the stream's
// MaxSubscribers cap is exceeded (caller should respond with 429).
func (str *Stream) addSubscriber(eventid int, url *url.URL) (*Subscriber, bool) {
	// Enforce MaxSubscribers cap before allocating anything.
	if str.MaxSubscribers > 0 && int(atomic.LoadInt32(&str.subscriberCount)) >= str.MaxSubscribers {
		return nil, false
	}

	atomic.AddInt32(&str.subscriberCount, 1)
	sub := &Subscriber{
		eventid:    eventid,
		quit:       str.deregister,
		streamQuit: str.quit,
		connection: make(chan *Event, 64),
		URL:        url,
	}

	if str.isAutoStream {
		sub.removed = make(chan struct{}, 1)
	}

	str.register <- sub

	if str.OnSubscribe != nil {
		go str.OnSubscribe(str.ID, sub)
	}

	str.mu.RLock()
	perStreamSub := str.streamOnSubscribe
	str.mu.RUnlock()
	if perStreamSub != nil {
		go perStreamSub(str.ID, sub)
	}

	return sub, true
}

func (str *Stream) removeSubscriber(i int) {
	atomic.AddInt32(&str.subscriberCount, -1)
	close(str.subscribers[i].connection)
	if str.subscribers[i].removed != nil {
		str.subscribers[i].removed <- struct{}{}
		close(str.subscribers[i].removed)
	}
	str.subscribers = append(str.subscribers[:i], str.subscribers[i+1:]...)
}

func (str *Stream) removeAllSubscribers() {
	for i := 0; i < len(str.subscribers); i++ { // nosemgrep: blocking-channel-send-in-iteration-loop
		close(str.subscribers[i].connection)
		if str.subscribers[i].removed != nil {
			// removed is a buffered channel (cap 1) and is guaranteed empty at
			// this point, so the send never blocks in practice.
			str.subscribers[i].removed <- struct{}{} // nosemgrep: blocking-channel-send-in-iteration-loop
			close(str.subscribers[i].removed)
		}
	}
	atomic.StoreInt32(&str.subscriberCount, 0)
	str.subscribers = str.subscribers[:0]
}

func (str *Stream) getSubscriberCount() int {
	return int(atomic.LoadInt32(&str.subscriberCount))
}

// SubscriberCount returns the current number of active subscribers on this stream.
// It is safe to call concurrently.
func (str *Stream) SubscriberCount() int {
	return str.getSubscriberCount()
}
