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

```bash
find . -type f -name '*.go' | xargs sed -i 's|github.com/r3labs/sse/v2|github.com/joshuafuller/sse/v3|g'
go get github.com/joshuafuller/sse/v3
```

v3 has **2 compile-breaking changes** (`EventLog` type, `Client.Connected` field)
and **6 behavioral differences** from v2. See **[docs/migrating-from-v2.md](docs/migrating-from-v2.md)**
for before/after code, the full behavioral change table, and new features.

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
