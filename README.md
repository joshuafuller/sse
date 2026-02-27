# SSE — Server Sent Events Client/Server Library for Go

A community-maintained fork of [r3labs/sse](https://github.com/r3labs/sse), which has
been inactive since January 2023. The goal is to fix known bugs, address the open issue
backlog, and keep the library current with modern Go. Full credit to the original
r3labs authors for the foundational work.

[![CI](https://github.com/joshuafuller/sse/actions/workflows/ci.yml/badge.svg)](https://github.com/joshuafuller/sse/actions/workflows/ci.yml)
[![Lint](https://github.com/joshuafuller/sse/actions/workflows/lint.yml/badge.svg)](https://github.com/joshuafuller/sse/actions/workflows/lint.yml)
[![codecov](https://codecov.io/gh/joshuafuller/sse/graph/badge.svg)](https://codecov.io/gh/joshuafuller/sse)
[![Go Reference](https://pkg.go.dev/badge/github.com/joshuafuller/sse/v3.svg)](https://pkg.go.dev/github.com/joshuafuller/sse/v3)
[![License: MPL 2.0](https://img.shields.io/badge/License-MPL_2.0-brightgreen.svg)](https://opensource.org/licenses/MPL-2.0)
[![Semgrep](https://github.com/joshuafuller/sse/actions/workflows/semgrep.yml/badge.svg)](https://github.com/joshuafuller/sse/actions/workflows/semgrep.yml)

## Changes from Upstream

This fork ships **40+ fixes** across the server, client, and parser — addressing
every open issue and unmerged pull request in the upstream
[r3labs/sse](https://github.com/r3labs/sse) tracker. All fixes are covered by
regression tests and verified with the race detector.

See **[CHANGELOG.md](CHANGELOG.md)** for the complete list with descriptions
and links to every upstream issue and PR.

## Migrating from r3labs/sse

Update your import path:

```bash
find . -type f -name '*.go' | xargs sed -i 's|github.com/r3labs/sse/v2|github.com/joshuafuller/sse/v3|g'
```

Then update your `go.mod`:

```bash
go get github.com/joshuafuller/sse/v3
```

### Breaking Changes in v3

#### `EventLog` type changed

`EventLog` was a named slice (`type EventLog []*Event`). It is now a struct with a configurable `MaxEntries` cap. Code that indexed, ranged over, or appended to `EventLog` directly will not compile.

**Before:**
```go
log := stream.Eventlog
for _, ev := range log {
    fmt.Println(ev.Data)
}
```

**After:**
```go
// EventLog is now opaque; events are replayed via Subscriber.connection
// or accessed through the AutoReplay mechanism on reconnect.
// To cap replay history, set MaxEntries when creating the stream:
server.CreateStream("messages")
stream := server.GetStream("messages")
stream.Eventlog.MaxEntries = 100
```

#### `Client.Connected` field removed

`Connected bool` was a public field on `*Client`. It is now an `atomic.Bool` accessed via the `Connected() bool` method. Direct field reads will not compile.

**Before:**
```go
if client.Connected {
    fmt.Println("connected")
}
```

**After:**
```go
if client.Connected() {
    fmt.Println("connected")
}
```

#### Behavioral changes to be aware of

| Behavior | v2 (r3labs) | v3 |
|----------|-------------|----|
| EOF / server close | treated as clean exit (no reconnect) | triggers reconnect |
| `io.ErrUnexpectedEOF` (keepalive timeout) | called `disconnectcb`, no reconnect | silent reconnect, `disconnectcb` not called |
| HTTP 204 No Content | reconnected | permanent stop (no reconnect) |
| `OnConnect` callback timing | fired on first event received | fires immediately after HTTP 200 |
| Non-200 HTTP responses | returned as `error` string | returned as `*StreamError`; inspect with `errors.As` |

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
