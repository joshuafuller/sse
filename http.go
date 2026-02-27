/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package sse

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// errWriter wraps an http.ResponseWriter and tracks the first write error.
// After any write fails, subsequent writes are no-ops.
type errWriter struct {
	w   http.ResponseWriter
	err error
}

func (ew *errWriter) printf(format string, args ...any) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprintf(ew.w, format, args...)
}

func (ew *errWriter) print(s string) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprint(ew.w, s)
}

// ServeHTTP serves new connections with events for a given stream ...
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}
	s.ServeHTTPWithFlusher(w, r, flusher)
}

// ServeHTTPWithFlusher serves SSE to frameworks that implement flushing
// separately from http.ResponseWriter (e.g. Fiber, fasthttp adapters).
// The flusher parameter must not be nil.
func (s *Server) ServeHTTPWithFlusher(w http.ResponseWriter, r *http.Request, flusher http.Flusher) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for k, v := range s.Headers {
		w.Header().Set(k, v)
	}

	// Get the StreamID from the URL
	streamID := r.URL.Query().Get("stream")
	if streamID == "" {
		http.Error(w, "Please specify a stream!", http.StatusInternalServerError)
		return
	}

	stream := s.getStream(streamID)

	if stream == nil {
		if !s.AutoStream {
			http.Error(w, "Stream not found!", http.StatusNotFound)
			return
		}

		stream = s.CreateStream(streamID)
	}

	eventid := 0
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		// Per WHATWG SSE spec (§9.2.4), Last-Event-ID is an opaque string.
		// If it parses as a number, use it for event log replay;
		// otherwise accept it without error (replay from the beginning).
		eventid, _ = strconv.Atoi(id)
	}

	// Create the stream subscriber; reject if the stream is at capacity.
	sub, ok := stream.addSubscriber(eventid, r.URL)
	if !ok {
		http.Error(w, "Too many subscribers!", http.StatusTooManyRequests)
		return
	}

	go func() {
		<-r.Context().Done()

		sub.close()

		if s.AutoStream && !s.AutoReplay && stream.getSubscriberCount() == 0 {
			s.RemoveStream(streamID)
		}
	}()

	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// If set, call OnSubscribeHTTP so the caller can write an initial event
	// (e.g. a "connected" message) before the main event loop begins.
	if s.OnSubscribeHTTP != nil {
		s.OnSubscribeHTTP(streamID, w)
	}

	// Push events to client
	ew := &errWriter{w: w}

	for ev := range sub.connection {
		// If the data buffer is an empty string, skip this event.
		if len(ev.Data) == 0 && len(ev.Comment) == 0 {
			continue
		}

		// if the event has expired, dont send it
		if s.EventTTL != 0 && time.Now().After(ev.timestamp.Add(s.EventTTL)) {
			continue
		}

		if len(ev.Data) > 0 {
			if len(ev.ID) > 0 {
				ew.printf("id: %s\n", ev.ID)
			}

			if s.SplitData {
				sd := bytes.Split(ev.Data, []byte("\n"))
				for i := range sd {
					ew.printf("data: %s\n", sd[i])
				}
			} else {
				if bytes.HasPrefix(ev.Data, []byte(":")) {
					ew.printf("%s\n", ev.Data)
				} else {
					ew.printf("data: %s\n", ev.Data)
				}
			}

			if len(ev.Event) > 0 {
				ew.printf("event: %s\n", ev.Event)
			}

			if len(ev.Retry) > 0 {
				ew.printf("retry: %s\n", ev.Retry)
			}
		}

		if len(ev.Comment) > 0 {
			ew.printf(": %s\n", ev.Comment)
		}

		ew.print("\n")

		flusher.Flush()

		if ew.err != nil {
			break
		}
	}
}
