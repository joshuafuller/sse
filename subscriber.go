/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import "net/url"

// Subscriber ...
type Subscriber struct {
	quit       chan *Subscriber
	connection chan *Event
	removed    chan struct{}
	// streamQuit is the stream's quit channel. close() selects on it so that
	// the context-done goroutine in ServeHTTP is never permanently blocked
	// when the stream's run() goroutine has already exited (NF-008 / sse-8er).
	streamQuit <-chan struct{}
	eventid    int
	URL        *url.URL
}

// close will let the stream know that the clients connection has terminated.
// If the stream has already shut down (streamQuit is closed) it returns
// immediately without blocking, preventing a goroutine leak.
func (s *Subscriber) close() {
	select {
	case s.quit <- s:
	case <-s.streamQuit:
		// Stream already shut down; nothing left to deregister.
		return
	}
	if s.removed != nil {
		<-s.removed
	}
}
