# SSE — Server Sent Events Client/Server Library for Go

A community-maintained fork of [r3labs/sse](https://github.com/r3labs/sse), which has
been inactive since January 2023. The goal is to fix known bugs, address the open issue
backlog, and keep the library current with modern Go. Full credit to the original
r3labs authors for the foundational work.

[![CI](https://github.com/joshuafuller/sse/actions/workflows/ci.yml/badge.svg)](https://github.com/joshuafuller/sse/actions/workflows/ci.yml)
[![Lint](https://github.com/joshuafuller/sse/actions/workflows/lint.yml/badge.svg)](https://github.com/joshuafuller/sse/actions/workflows/lint.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/joshuafuller/sse/v3.svg)](https://pkg.go.dev/github.com/joshuafuller/sse/v3)
[![License: MPL 2.0](https://img.shields.io/badge/License-MPL_2.0-brightgreen.svg)](https://opensource.org/licenses/MPL-2.0)

## Changes from Upstream

All fixes are covered by regression tests and verified with the race detector.

| Area | Change | Upstream Issue |
|------|--------|----------------|
| **Server** | Non-blocking fan-out dispatch — slow subscribers no longer stall the entire stream | — |
| **Server** | `id:` field omitted when event has no ID; previously always emitted, corrupting client LastEventID | — |
| **Server** | Missing stream with `AutoStream=false` returns HTTP 404 instead of 500 | [#33](https://github.com/r3labs/sse/issues/33) |
| **Server** | `Last-Event-ID` request header accepted as opaque string; previously rejected non-numeric values with 400 | [#33](https://github.com/r3labs/sse/issues/33) |
| **Server** | Empty-data events use `continue` instead of `break` — subscriber loop no longer terminates early | [#163](https://github.com/r3labs/sse/issues/163) |
| **Server** | All `fmt.Fprintf` write errors in `ServeHTTP` are now checked; loop exits on client disconnect | — |
| **Server** | `EventLog` bounded by configurable `MaxEntries`; previously grew unboundedly with `AutoReplay` | [#169](https://github.com/r3labs/sse/issues/169) |
| **Server** | Per-stream `SetOnSubscribe`/`SetOnUnsubscribe` callbacks available on `*Stream` | [#138](https://github.com/r3labs/sse/issues/138) |
| **Server** | `OnSubscribeHTTP` callback exposes `http.ResponseWriter` to send initial messages at connect time | [#177](https://github.com/r3labs/sse/issues/177) |
| **Server** | `MaxSubscribers` field on `*Stream`; returns 429 when exceeded | [#172](https://github.com/r3labs/sse/issues/172) |
| **Server** | `ServeHTTPWithFlusher` method for Fiber/fasthttp compatibility | [#139](https://github.com/r3labs/sse/issues/139) |
| **Client** | EOF and server-close now trigger reconnect instead of treating stream end as success | [#76](https://github.com/r3labs/sse/issues/76) |
| **Client** | HTTP 204 No Content causes permanent stop (no reconnect) | — |
| **Client** | `Content-Type` validated on response; non-`text/event-stream` fails permanently | — |
| **Client** | `retry:` field value applied as reconnect delay in milliseconds | — |
| **Client** | `id:` with empty value correctly resets `LastEventID` (vs. absent `id:` which preserves it) | — |
| **Client** | `id:` field value containing U+0000 NULL is ignored per spec | — |
| **Client** | `SubscribeWithContext` returns `context.Canceled` when context is cancelled | [#131](https://github.com/r3labs/sse/issues/131) |
| **Client** | `erChan` buffered (`size=1`) — `readLoop` goroutine no longer leaks when consumer exits early | — |
| **Client** | Scanner buffer overflow returns `bufio.ErrTooLong` instead of `io.EOF` | [#158](https://github.com/r3labs/sse/issues/158) |
| **Client** | ID-only events (no `data:`) not dispatched to handler per WHATWG spec | [#163](https://github.com/r3labs/sse/issues/163) |
| **Parser** | Leading UTF-8 BOM stripped from event stream before parsing | — |
| **Testing** | `TestSubscribeWithContextDone` rewritten with event-driven sync; no more timing-based flakiness | [#186](https://github.com/r3labs/sse/issues/186) |
| **Client** | `OnConnect` callback fires immediately after HTTP 200 response instead of on first event | [#149](https://github.com/r3labs/sse/issues/149) |
| **Client** | `io.ErrUnexpectedEOF` (keepalive timeout) triggers silent reconnect; `disconnectcb` not called spuriously | — |
| **Client** | Non-200 responses return `*StreamError` with `StatusCode` and `Body` fields; inspectable via `errors.As` | [#168](https://github.com/r3labs/sse/issues/168) |
| **Client** | Backoff resets after each successful connection; next reconnect starts from minimum delay | — |
| **Client** | Context passed into `startReadLoop`; goroutine exits promptly on context cancel | — |
| **Client** | External errors wrapped with `%w` for `errors.Is`/`errors.As` compatibility | — |
| **Client** | `Method` and `Body` fields added; supports POST and any HTTP method with a per-attempt body factory | [#153](https://github.com/r3labs/sse/issues/153) |

## Migrating from r3labs/sse

Update your import path:

```bash
find . -type f -name '*.go' | xargs sed -i 's|github.com/r3labs/sse/v2|github.com/joshuafuller/sse/v3|g'
```

Then update your `go.mod`:

```bash
go get github.com/joshuafuller/sse/v3
```

## Quick Start

```bash
go get github.com/joshuafuller/sse/v3
```

## Example Server

There are two parts of the server: the message scheduler and an HTTP handler.

Start the scheduler:

```go
func main() {
	server := sse.New()
}
```

Add a stream:

```go
func main() {
	server := sse.New()
	server.CreateStream("messages")
}
```

Clients connect by specifying _stream_ as a URL parameter:

```
http://server/events?stream=messages
```

Start the HTTP server:

```go
func main() {
	server := sse.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", server.ServeHTTP)

	http.ListenAndServe(":8080", mux)
}
```

Publish to a stream:

```go
server.Publish("messages", &sse.Event{
	Data: []byte("ping"),
})
```

Detect disconnected clients:

```go
mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
	go func() {
		<-r.Context().Done()
		println("client disconnected")
	}()
	server.ServeHTTP(w, r)
})
```

## Example Client

Create a client:

```go
client := sse.NewClient("http://server/events")
```

Subscribe with a handler:

```go
client.Subscribe("messages", func(msg *sse.Event) {
	fmt.Println(msg.Data)
})
```

Subscribe to a channel:

```go
events := make(chan *sse.Event)
client.SubscribeChan("messages", events)
```

Configure the HTTP transport (e.g. disable TLS verification):

```go
client.Connection.Transport = &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
}
```

Use custom URL query parameters:

```go
client := sse.NewClient("http://server/events?search=example")
client.SubscribeRaw(func(msg *sse.Event) {
	fmt.Println(msg.Data)
})
```

## Contributing

Pull requests are welcome. A few guidelines:

- All bug fixes and features must include tests written **before** the implementation (TDD)
- If you previously submitted a PR against [r3labs/sse](https://github.com/r3labs/sse) that was never merged, please re-submit it here
- Open an issue before starting large changes so we can discuss approach

## Versioning

This project follows [Semantic Versioning](http://semver.org/). This fork begins at v3.0.0.

## Copyright and License

Code and documentation copyright 2015 r3labs.io authors.
Modifications copyright 2026 Joshua Fuller.

Code released under the [Mozilla Public License Version 2.0](LICENSE).
