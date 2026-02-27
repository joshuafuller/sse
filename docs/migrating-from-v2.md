# Migrating from r3labs/sse (v2 → v3)

## Import path

```bash
find . -type f -name '*.go' | xargs sed -i 's|github.com/r3labs/sse/v2|github.com/joshuafuller/sse/v3|g'
go get github.com/joshuafuller/sse/v3
```

## Breaking changes

### 1. `EventLog` type changed

`EventLog` was a named slice (`type EventLog []*Event`). It is now a struct
with a configurable `MaxEntries` cap. Code that indexed, ranged over, or
appended to `EventLog` directly will not compile.

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

### 2. `Client.Connected` field removed

`Connected bool` was a public field on `*Client`. It is now an `atomic.Bool`
accessed via the `Connected() bool` method. Direct field reads will not compile.

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

## Behavioral changes

| Behavior | v2 (r3labs) | v3 |
|----------|-------------|----|
| EOF / server close | treated as clean exit (no reconnect) | triggers reconnect |
| `io.ErrUnexpectedEOF` (keepalive timeout) | called `disconnectcb`, no reconnect | silent reconnect, `disconnectcb` not called |
| HTTP 204 No Content | reconnected | permanent stop (no reconnect) |
| `OnConnect` callback timing | fired on first event received | fires immediately after HTTP 200 |
| Non-200 HTTP responses | returned as `error` string | returned as `*StreamError`; inspect with `errors.As` |
| `OnDisconnect` / `Connected()` on cancel | not fired / not cleared | fired and cleared when subscription exits |

## New features in v3

- `Client.Connected() bool` — thread-safe connection state
- `Client.OnConnect(func(*Client))` / `Client.OnDisconnect(func(*Client))` — lifecycle callbacks
- `StreamError` type — structured non-200 errors with `StatusCode` and `Body`
- `Server.TryPublish` — non-blocking publish (drops if full)
- `Server.GetStream` — retrieve a stream by ID
- `Stream.SubscriberCount()` — live subscriber count
- `Stream.SetOnSubscribe` / `Stream.SetOnUnsubscribe` — per-stream callbacks
- `Stream.Eventlog.MaxEntries` — cap the replay log size
- `ErrEmptyEventMessage` — exported sentinel for empty-event parse errors
- `Server.ServeHTTPWithFlusher` — adapter for frameworks with separate flusher
- UTF-8 BOM stripping, scanner buffer overflow handling, id/retry spec compliance
