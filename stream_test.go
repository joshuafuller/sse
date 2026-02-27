/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// Package sse — white-box test file (sse-4pn).
// Cannot migrate to package sse_test because the tests access unexported
// identifiers: newStream (func), Stream.addSubscriber (method),
// Stream.deregister (field), Subscriber.connection (field),
// Stream.getSubscriberCount (method).
package sse

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests are accessing subscriber in a non-threadsafe way.
// Maybe fix this in the future so we can test with -race enabled

func TestStreamAddSubscriber(t *testing.T) {
	s := newStream("test", 1024, true, false, nil, nil)
	s.run()
	defer s.close()

	s.event <- &Event{Data: []byte("test")}
	sub := s.addSubscriber(0, nil)

	assert.Equal(t, 1, s.getSubscriberCount())

	s.event <- &Event{Data: []byte("test")}
	msg, err := wait(sub.connection, time.Second*1)

	require.Nil(t, err)
	assert.Equal(t, []byte(`test`), msg)
	assert.Equal(t, 0, len(sub.connection))
}

func TestStreamRemoveSubscriber(t *testing.T) {
	s := newStream("test", 1024, true, false, nil, nil)
	s.run()
	defer s.close()

	sub := s.addSubscriber(0, nil)
	time.Sleep(time.Millisecond * 100)
	s.deregister <- sub
	time.Sleep(time.Millisecond * 100)

	assert.Equal(t, 0, s.getSubscriberCount())
}

func TestStreamSubscriberClose(t *testing.T) {
	s := newStream("test", 1024, true, false, nil, nil)
	s.run()
	defer s.close()

	sub := s.addSubscriber(0, nil)
	sub.close()
	time.Sleep(time.Millisecond * 100)

	assert.Equal(t, 0, s.getSubscriberCount())
}

func TestStreamDisableAutoReplay(t *testing.T) {
	s := newStream("test", 1024, true, false, nil, nil)
	s.run()
	defer s.close()

	s.AutoReplay = false
	s.event <- &Event{Data: []byte("test")}
	time.Sleep(time.Millisecond * 100)
	sub := s.addSubscriber(0, nil)

	assert.Equal(t, 0, len(sub.connection))
}

func TestStreamSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	s := newStream("test", 1024, false, false, nil, nil)
	s.run()
	defer s.close()

	slowSub := s.addSubscriber(0, nil)
	fastSub := s.addSubscriber(0, nil)

	// Wait for both subscribers to be registered
	time.Sleep(time.Millisecond * 100)

	// Fill the slow subscriber's channel to capacity so sends to it would block
	for i := 0; i < cap(slowSub.connection); i++ {
		slowSub.connection <- &Event{Data: []byte("filler")}
	}

	// Now publish an event — this must not block on the slow subscriber
	s.event <- &Event{Data: []byte("important")}

	// The fast subscriber should receive the event promptly
	select {
	case ev := <-fastSub.connection:
		assert.Equal(t, []byte("important"), ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("fast subscriber blocked by slow subscriber — dispatch is not non-blocking")
	}
}

func TestStreamMultipleSubscribers(t *testing.T) {
	var subs []*Subscriber

	s := newStream("test", 1024, true, false, nil, nil)
	s.run()

	for i := 0; i < 10; i++ {
		subs = append(subs, s.addSubscriber(0, nil))
	}

	// Wait for all subscribers to be added
	time.Sleep(time.Millisecond * 100)

	s.event <- &Event{Data: []byte("test")}
	for _, sub := range subs {
		msg, err := wait(sub.connection, time.Second*1)
		require.Nil(t, err)
		assert.Equal(t, []byte(`test`), msg)
	}

	s.close()

	// Wait for all subscribers to close
	time.Sleep(time.Millisecond * 100)
	assert.Equal(t, 0, s.getSubscriberCount())

}
