# Article Brief — Connection Pooling Deep Dive
## (Follow-up to: Go's Netpoller and TCP Connection Lifecycle)

---

## Context for Claude Desktop

This is the second article in a series. The first article covered:
- File descriptors as kernel resources
- TCP socket syscalls: `socket()`, `bind()`, `listen()`, `accept()`
- Go's netpoller: goroutines parked as `pollDesc` entries, epoll-driven wakeups
- Connection teardown cost: epoll removal, FIN/ACK, TIME_WAIT (up to 120s), fd table cleanup
- Closing line: *"While Go's netpoller solves thread efficiency, pools address the underlying connection cost problem."*

**This article picks up from that closing line.** The reader already understands why connections are expensive. Now we go deep on how a pool actually works — built from scratch.

**Writing style:** Technical depth, first-principles, concrete kernel grounding. Wire-level, not framework-level. Show the actual code and actual log output. Same tone as the previous article.

**Target reader:** Go developer who uses `http.Client` daily but has never thought about what's underneath.

---

## Opening Bridge

The netpoller freed your threads. But every new TCP connection still pays a price — a syscall, a handshake RTT, and a TIME_WAIT slot that occupies kernel memory for up to 120 seconds on teardown.

The answer is reuse. But how does a pool actually work?

---

## Article Outline

### 1. The server decides first

Connection pooling is not a client invention. It requires an explicit server decision.

A naive server closes after every response:
```
Accept → handle request → write response → conn.Close()
```

A pooling-aware server loops instead:
```
Accept → for sc.Scan() { handle request → write response } → (loop)
```

The `for sc.Scan()` is the deliberate choice. The server goroutine stays parked on the netpoller between requests — no OS thread consumed, connection held open. This is what gives the client something to reuse.

The loop exits only when:
- Client disconnects → `sc.Scan()` returns false (EOF received)
- `IdleConnTimeout` fires on the pool side → fd closed, FIN sent → server wakes

By not closing, the server participates in the pooling contract. The goroutine stays alive, parked, consuming no OS thread — the same netpoller magic from the previous article.

---

### 2. The framing contract — inseparable from pooling

The server staying in the loop creates a fundamental problem: how does it know where one request ends and the next begins?

**Client → Server (query delimiting):**
The server's `sc.Scan()` loop uses `\n` as a message boundary. Real databases use length-prefixed frames or special terminator bytes.

**Server → Client (response delimiting):**
The client needs a sentinel — a "response complete" signal — before it can safely return the connection to the pool. Return it mid-response and the next caller gets a connection with leftover bytes in the recv buffer.

How different systems solve this:
- **Postgres:** `CommandComplete` + `ReadyForQuery` frames. `ReadyForQuery` = "full response delivered, connection is idle."
- **HTTP with Content-Length:** client reads exactly N bytes, done.
- **HTTP chunked:** `0\r\n` terminating chunk signals end of stream.

**Pooling and framing are inseparable.** Without the sentinel, the pool can't know when a connection is safe to reuse. This is why database drivers and HTTP clients always implement a framing protocol alongside the pool — they're two halves of the same contract.

---

### 3. Building the simplest pool — a buffered channel

```go
type ConnPool struct {
    pool chan net.Conn  // capacity = MaxConn
    Addr string
}

func (p *ConnPool) GetConn() (net.Conn, error) {
    select {
    case conn := <-p.pool:
        return conn, nil
    default:
        return net.Dial("tcp", p.Addr)
    }
}

func (p *ConnPool) ReturnConn(c net.Conn) {
    select {
    case p.pool <- c:
    default:
        c.Close() // pool full — close, never drop (fd leak)
    }
}
```

The `default` case in both is key:
- `GetConn`: pool empty → dial fresh
- `ReturnConn`: pool full → `c.Close()` (not drop — or the fd and kernel socket struct leak)

A buffered channel is the synchronization primitive. No mutex needed — channels are already thread-safe. Adding a `sync.Mutex` on top is redundant.

**Proof of reuse — same ephemeral port:**
```
iter[0]: conn.LocalAddress(127.0.0.1:55645)
iter[1]: conn.LocalAddress(127.0.0.1:55645)  ← same port, same tcp_sock, zero new handshakes
```
The kernel assigns one ephemeral port per TCP connection. Same port across two `GetConn` calls = same `net.Conn` returned from pool = zero new dials.

**The overflow demo (pool=3, 5 connections):**
```
Server output:
[go-routine-2][127.0.0.1:55645]: connected
[go-routine-2][127.0.0.1:55645]: [0]: hello     ← request 1
[go-routine-2][127.0.0.1:55645]: [0]: hello     ← request 2, same goroutine, same conn
[go-routine-0][127.0.0.1:55643]: [3]: hello
[go-routine-0][127.0.0.1:55643]: disconnected   ← immediately, ReturnConn closed it (pool full)
[go-routine-2][127.0.0.1:55645]: disconnected   ← on process exit
```
go-routine-2 handled two separate request-response cycles on the same connection — this is connection reuse in action. go-routine-0 disconnected immediately because the pool was already full when it tried to return.

---

### 4. Adding knobs — semaphore for MaxConnsPerHost

A buffered channel pool has no burst protection. 1000 goroutines hitting `GetConn` simultaneously dial 1000 connections to the server at once.

Fix: a semaphore (also a `chan struct{}`) pre-filled with `MaxConnsPerHost` tokens.

```go
// semaphore pre-filled: N tokens = N simultaneous connections allowed
maxConnsPerHost := make(chan struct{}, MaxConnsPerHost)
for i := 0; i < MaxConnsPerHost; i++ {
    maxConnsPerHost <- struct{}{}
}

// GetConn: must acquire a token before dialing or getting from pool
<-p.MaxConnsPerHost  // blocks when at limit — parks goroutine, no spin

// ReturnConn: release token when connection is closed (not when returned to pool)
p.MaxConnsPerHost <- struct{}{}
```

`chan struct{}` not `chan int` — `struct{}` is zero-size, no allocation per token.

**Live demo: 10 goroutines, MaxConnsPerHost=6**

```
02:26:36 [go-routine-6] blocking for MaxConnsPerHost   ← 4 parked immediately
02:26:36 [go-routine-7] blocking for MaxConnsPerHost
02:26:36 [go-routine-8] blocking for MaxConnsPerHost
02:26:36 [go-routine-4] blocking for MaxConnsPerHost
02:26:36 [go-routine-1] using conn.LocalAddress(127.0.0.1:58963)   ← 6 dial
02:26:36 [go-routine-3] using conn.LocalAddress(127.0.0.1:58964)
02:26:36 [go-routine-2] using conn.LocalAddress(127.0.0.1:58966)
02:26:36 [go-routine-5] using conn.LocalAddress(127.0.0.1:58967)
02:26:36 [go-routine-0] using conn.LocalAddress(127.0.0.1:58965)
02:26:36 [go-routine-9] using conn.LocalAddress(127.0.0.1:58968)
                                                         ← 2 seconds later
02:26:38 [go-routine-6] using conn.LocalAddress(127.0.0.1:58971)   ← 3 unblock
02:26:38 [go-routine-8] using conn.LocalAddress(127.0.0.1:58970)
02:26:38 [go-routine-7] using conn.LocalAddress(127.0.0.1:58969)
```

Wave pattern: burst hits cap → excess goroutines park on the semaphore → as in-flight connections complete and return, parked goroutines unblock in waves.

---

### 5. IdleConnTimeout — evicting stale connections

Idle connections sitting forever create a problem: servers close them after their own timeout (sending a FIN). Client grabs the stale connection, writes a request, gets an error.

Fix: timestamp each connection on return. A background goroutine wakes periodically and evicts connections that have been idle too long.

```go
type ConnPool struct {
    mu   sync.Mutex
    pool []struct {
        idleSince time.Time
        conn      net.Conn
    }
    MaxIdleConnsPerHost int
    MaxConnsPerHost     chan struct{}
    IdleConnTimeout     time.Duration
}
```

**Why switch from channel to slice:**
A buffered channel can't be peeked without consuming. To check expiry, you need to inspect elements without removing them. A slice as a FIFO queue works: append at the back (newest last), pop from back for `GetConn` (warmest first), check the front for eviction (oldest first).

**Eviction interval:** `IdleConnTimeout / 2`. Worst case: connection becomes idle just after a wakeup. One interval later it's checked — age is `IdleConnTimeout / 2`. One more interval → evicted. Maximum overshoot: one interval. Go's `net/http` uses exactly this internally.

**Always set `IdleConnTimeout` below the server's timeout.** If the server times out first and sends a FIN, the client's next attempt on that stale connection will error.

---

### 6. Dedicated-host vs shared-host — two mindset switches

**Dedicated-host pool:** one pool per target server. All knobs scope to that one host. Simple, predictable.

**Shared-host pool:** one pool for all hosts. `map[addr] → []conn`. Two structural changes required:

**Mindset switch 1 — per-host resource → global resource:**
`MaxConnsPerHost` becomes `map[addr] → chan struct{}` — one independent semaphore per host. Now a second scope appears: `MaxIdleConns` (global) — caps total idle connections across all hosts, protecting the client process from fd exhaustion.

**Mindset switch 2 — connections → idle slot tokens:**
The global `IdleConns` channel is a semaphore for idle slots, not connections. The invariant:

```
tokens_in_IdleConns + connections_in_pool = MaxIdleConns  (always)
```

| Operation | `MaxConns[addr]` | `IdleConns` global |
|---|---|---|
| Dial new conn | consume 1 | no change |
| ReturnConn → pool | no change | consume 1 (entering idle) |
| ReturnConn → per-host cap | release 1 (closed) | no change |
| ReturnConn → global cap | release 1 (closed) | no change |
| GetConn → pool hit | no change | release 1 (leaving idle) |
| Eviction | release 1 (closed) | release 1 (leaving idle) |

**Eviction must release both** — an evicted connection held a per-host slot (consumed at dial) and a global idle slot (consumed at ReturnConn). Releasing only one causes permanent token starvation.

**Non-determinism:** Which host accumulates idle slots under the global cap is non-deterministic — it depends on goroutine scheduling. Two runs with identical config:
- Run 1: host A gets 2 slots, host B gets 2, host C gets 1
- Run 2: host A gets 3 slots, host B gets 1, host C gets 1

Same invariant holds either way: total never exceeds `MaxIdleConns`.

---

### 7. The knobs — what each one does

**`MaxConnsPerHost`** — server protection + burst absorption. Caps simultaneous connections to one server. Set higher than `MaxIdleConnsPerHost`: extra connections created under burst get closed on return (pool already full), naturally draining back to the idle cap.

**`MaxIdleConnsPerHost`** — caps stale idle accumulation per host. Without it, a burst of 100 connections leaves 100 idle fds sitting in the pool.

**`MaxIdleConns` (global)** — client process resource ceiling. A client talking to 50 hosts with `MaxIdleConnsPerHost=10` could hold 500 idle fds — most unused. Global cap bounds the total regardless of host count.

**No global `MaxConns`** — a global connection limit across all hosts has no coherent meaning. Throttling connections to `api.stripe.com` because you have too many open to `api.github.com` protects neither server.

**SQLAlchemy / Airflow analogy:**
```yaml
AIRFLOW__DATABASE__SQL_ALCHEMY_POOL_SIZE: "20"     # = MaxIdleConnsPerHost
AIRFLOW__DATABASE__SQL_ALCHEMY_MAX_OVERFLOW: "20"  # = burst headroom
# MaxConnsPerHost = POOL_SIZE + MAX_OVERFLOW = 40 total
```
Same semaphore pattern, same wave behaviour, different language.

---

### 8. HTTP framing — what net/http does transparently

HTTP is the same pool, with a self-describing framing protocol.

**Raw TCP framing:**
- Request end: `\n`
- Response end: implicit — one line echoed back
- Connection fate: server loop stays open

**HTTP framing:**
- Request end: `\r\n\r\n` after headers + `Content-Length` bytes
- Response end: `Content-Length` bytes read OR `0\r\n\r\n` in chunked encoding
- Connection fate: `Connection` header decides

`http.Transport` is a shared-host pool. The same knobs map directly:
- `MaxIdleConnsPerHost`, `MaxIdleConns`, `MaxConnsPerHost`, `IdleConnTimeout`

**HTTP/1.1 default is keep-alive.** `Connection: close` is the opt-out. `Connection: keep-alive` is redundant. Proof: removing it from the server changed nothing observable in pool behaviour.

**`resp.Body.Close()` is not optional.** Without draining and closing the body, `http.Transport` doesn't know the response is complete — the connection can't be returned to the pool. It leaks.

---

### 9. HOL blocking — a consequence of pool design

HTTP/1.1 with keep-alive: one connection, one request in-flight at a time.

```
22:54:46  ConnectStart → :53594          ← one dial, one connection
22:54:46  GotConn :53594 → fast          ← instant
22:54:46  GotConn :53594 → fast
22:54:46  GotConn :53594 → fast
22:54:46  GotConn :53594 → fast
22:54:46  GotConn :53594 → [waiting]     ← slow request holds the connection
22:54:51  response: slow                 ← 5 seconds later
22:54:51  GotConn :53594 → fast          ← all unblock instantly
```

One slow request blocked five fast requests — not because the server was busy but because they all shared one connection with no way to label which response belongs to which request.

**The root cause:** ordering without identity. The TCP byte stream has no per-request label. Responses must come back in the same order as requests. Fix: HTTP/2 adds a 31-bit stream ID to every frame — the receiver reassembles each response independently. One connection, N concurrent streams, no HOL at the application layer.

The pool design determines HOL exposure: one idle connection → full HOL, more connections → isolated per-connection, HTTP/2 → eliminated at app layer (TCP HOL remains).

---

## What to ask Claude Desktop

Use the outline above + this context. Suggested prompt:

> Write a technical Substack article as a follow-up to the attached previous article. Match the depth and tone exactly — kernel-level grounding, concrete code and log output, no hand-waving.
>
> Use this outline: [paste outline]
>
> Previous article for tone reference: [paste previous article text]
>
> The article should read as a continuous learning journey — "I built this from scratch and here's what I discovered at each step." Not a tutorial. Not a reference doc. A story with technical depth.