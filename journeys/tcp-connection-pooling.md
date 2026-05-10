# Journey Document: TCP Connection Pooling in Go

> Structured brain dump to be shaped into a Substack article.
> Continues from: TCP Echo Server deep dive (journeys/tcp-echo-server.md)

---

## Why This Topic

After understanding the full lifecycle of a TCP connection — socket syscalls, kernel queues, handshake, recv/send buffers, pollDesc, epoll — the cost of creating a new connection becomes concrete, not abstract.

**Every new connection involves:**

Server side:
- New `tcp_sock` struct + fd allocation in kernel RAM
- New recv/send buffers
- `pollDesc` entry registered via `epoll_ctl`
- 3-way handshake (SYN → SYN-ACK → ACK) = minimum 1 RTT

Client side:
- New `tcp_sock` struct + fd allocation
- New recv/send buffers
- `pollDesc` entry registered via `epoll_ctl`
- If TLS: add 1-2 more RTTs for the TLS handshake

At high request rates, paying this cost per request creates two problems:
1. **Latency** — caller waits for handshake before the first byte of their request goes out
2. **Memory** — RAM consumed by socket structs and buffers adds up fast

Connection pooling solves this by keeping established connections alive and reusing them.

---

## Mental Model: What a Pool Is

Same principle as buffer pooling (sync.Pool, zerolog) — create once, reuse many times, avoid the allocation cost.

For TCP: keep connections in ESTABLISHED state between requests. No handshake. No buffer allocation. No epoll registration. Just grab an existing fd and write.

**Pool states:**
- **Idle** — connection is ESTABLISHED, sitting in pool, ready to grab
- **In-use** — grabbed by a request, removed from pool during the request, returned when done

---

## Core Insights — The Aha Moments

### 1. Pooling starts with a server decision

Connection pooling is not a client invention. It requires an explicit server choice.

A naive server closes the connection after every response:
```
Accept → handle request → write response → conn.Close()
```

A pooling-aware server does the opposite — it stays in a loop, waiting for the next request on the same connection:
```
Accept → for sc.Scan() { handle request → write response } → (loop)
```

The `for sc.Scan()` is the deliberate decision. The server goroutine stays parked on the netpoller between requests — no OS thread consumed, connection held open. This is what gives the client something to reuse.

The server only exits the loop (and closes the connection) when:
- The client disconnects → `sc.Scan()` returns false (EOF)
- `IdleConnTimeout` fires → eviction goroutine closes the fd

By **not closing**, the server lets the client leverage pooling — improving both latency (no repeated handshake) and resource utilization (fewer fds, fewer kernel socket structs). The server benefits too: its own goroutines stay parked rather than spawning new ones per connection.

### 2. The framing contract — inseparable from pooling

The server staying in the loop creates a fundamental problem: how does it know where one request ends? How does the client know when the full response has arrived and it's safe to return the connection to the pool?

**Client → Server (query delimiting):**
The server's read loop needs a boundary to know "stop reading, start executing." `sc.Scan()` uses `\n`. Real databases use length-prefixed frames or special terminator bytes.

**Server → Client (response delimiting):**
The client needs a sentinel — a "response complete" signal — before it can return the connection. Return it mid-response and the next caller gets a connection with leftover bytes in the recv buffer. Corrupted data.

How different systems solve this:
- **Postgres:** `CommandComplete` + `ReadyForQuery` frames. `ReadyForQuery` means "full response delivered, connection is idle."
- **HTTP chunked:** `0\r\n` terminating chunk — zero-length chunk signals "stream over."
- **HTTP with Content-Length:** client reads exactly N bytes, knows it's done.

**Pooling and framing are inseparable.** Without the sentinel, the pool can't know when a connection is safe to reuse. This is why database drivers and raw TCP clients always implement a framing protocol alongside the pool — they're two halves of the same contract.

### 3. Why each knob exists

**`MaxConnsPerHost`** — server protection + burst absorption. Without it, 1000 goroutines could dial simultaneously. The per-host semaphore caps live connections to one server. Set it higher than `MaxIdleConnsPerHost` to absorb burst: extra connections are created under load, then closed on `ReturnConn` (pool already full), naturally draining back to the idle cap.

**`MaxIdleConnsPerHost`** — caps stale idle accumulation per host. After a burst of 100 connections, without this cap, all 100 would sit idle in the pool consuming fds and RAM on both sides.

**`MaxIdleConns` (global)** — client process resource ceiling. A client talking to 50 hosts with `MaxIdleConnsPerHost = 10` could hold 500 idle fds — most unused. The global cap bounds total idle connections regardless of how many hosts are involved.

**No global `MaxConns`** — a global connection limit across all hosts has no coherent meaning. Throttling connections to `api.stripe.com` because you have too many open to `api.github.com` protects neither server. Overload is per-server; the protection belongs at the per-host level.

### 4. Shared-host vs dedicated-host — the structural difference

**Dedicated-host pool:** `Addr` hardcoded at construction. One pool instance per target. All knobs scope naturally to that one server. Simple, precise, no cross-host interference.

**Shared-host pool:** one pool instance, all hosts. Connections keyed by addr → `map[addr] → []conn`. `MaxConnsPerHost` becomes `map[addr] → chan struct{}` — one independent semaphore per host. Now *two* idle scopes matter: per-host (protects one server from stale accumulation) and global (protects the client process from fd exhaustion across many hosts).

No global `MaxConns` in the shared pool — same reasoning as above. Global `MaxIdleConns` yes — because idle connections are a client-side resource concern, and the total matters regardless of which host they belong to.

`net/http`'s `Transport` is a shared-host pool. Microservice pattern: dedicated client for high-traffic services (precise per-service tuning), shared client for everything else.

### 5. Semaphore synchronization — the invariant

Three operations touch both semaphores (`IdleConns` global, `MaxConns[addr]` per-host). The core invariant:

```
tokens_in_IdleConns + connections_in_pool = MaxIdleConns  (always)
```

| Operation | `MaxConns[addr]` | `IdleConns` global |
|---|---|---|
| Dial new conn | consume 1 (acquire slot) | no change |
| `ReturnConn` → pool | no change | consume 1 (entering idle) |
| `ReturnConn` → per-host cap | release 1 (conn closed) | no change |
| `ReturnConn` → global cap | release 1 (conn closed) | no change |
| `GetConn` → pool hit | no change | release 1 (leaving idle) |
| Eviction | release 1 (conn closed) | release 1 (leaving idle) |

**Eviction must release both** — an evicted connection held a per-host slot (consumed at dial) and a global idle slot (consumed at `ReturnConn`). Releasing only one causes permanent token starvation.

**Why `GetConn`'s idle release never blocks:** a connection is only in the pool if `ReturnConn` consumed an `IdleConns` token to put it there. `tokens_in_IdleConns ≤ MaxIdleConns - 1` at that point. The channel always has room. The release is guaranteed non-blocking — not by luck, but by the invariant.

---

## The Knobs — net/http Transport

`http.Transport` exposes four key knobs, all discovered via VS Code's struct fill quick fix:

- **`MaxIdleConnsPerHost`** — warm idle connections per host. Default: `2` (very conservative). First thing to tune under high load.
- **`MaxIdleConns`** — global ceiling across all hosts. Only relevant when one `http.Client` talks to multiple hosts.
- **`MaxConnsPerHost`** — hard ceiling on total connections (idle + in-use). When hit, new requests block waiting. Default: `0` (no limit).
- **`IdleConnTimeout`** — evict idle connections after this duration. Default: `0` (no limit — connections sit forever, vulnerable to server-side RST).

**Key insight on defaults:** `IdleConnTimeout = 0` means idle connections never expire client-side. If the server has its own idle timeout, it sends a FIN/RST. Client grabs that stale connection from the pool, writes a request, gets an error. Always set `IdleConnTimeout` explicitly, below the server's idle timeout.

### Real-world mapping: SQLAlchemy / Airflow

I recently was working on Airflow where many parallel DAG tasks were connecting to Postgres simultaneously. The default config was throwing connection errors — too many tasks trying to connect at once.

The fix was tuning two SQLAlchemy knobs in `docker-compose`:

```yaml
AIRFLOW__DATABASE__SQL_ALCHEMY_POOL_SIZE: "20"
AIRFLOW__DATABASE__SQL_ALCHEMY_MAX_OVERFLOW: "20"
```

These map directly to the same concepts:

- `POOL_SIZE` = `MaxIdleConnsPerHost` — idle connections kept ready
- `MAX_OVERFLOW` = extra connections allowed beyond pool size
- `MaxConnsPerHost` = `POOL_SIZE + MAX_OVERFLOW` = 20 + 20 = 40 total

Airflow defaults: `POOL_SIZE = 5`, `MAX_OVERFLOW = 5` → 10 total. Under parallel DAG execution that exhausts fast. Same concept, different names.

### Why each knob exists

- **Capping idle connections (`MaxIdleConnsPerHost`)** — avoids many stale connections sitting in the pool consuming fds and RAM on both sides when traffic is low.
- **Capping total connections (`MaxConnsPerHost`)** — protects the server from being overwhelmed. Set higher than `MaxIdleConnsPerHost` to handle burst traffic — extra connections get created under load, then closed when returned (pool already full), naturally draining back to the idle cap.

### Client-per-host pattern vs shared client

In a microservice environment talking to many downstream services:
- **Dedicated `http.Client` per high-traffic service** — tuned `MaxIdleConnsPerHost`, `MaxConnsPerHost`, custom timeouts. `MaxIdleConns` is irrelevant (one host).
- **Shared `http.Client` for low-traffic services** — now both `MaxIdleConns` and `MaxIdleConnsPerHost` matter. `MaxIdleConns` caps total; `MaxIdleConnsPerHost` prevents any one service from hogging the shared pool.

Trade-off: dedicated clients are clean but idle connections on rarely-called services waste RAM and fds.

### Why connections can be returned to pool — the framing contract

`net/http` knows a connection is safe to reuse after a response when:
- `Content-Length` bytes have been fully read, OR
- Final zero-length chunk received (`0\r\n`) in `Transfer-Encoding: chunked`

**This is why `resp.Body.Close()` is critical.** Without draining and closing the body, the transport doesn't know you're done — the connection leaks from the pool.

Verified via `curl --raw` on the SSE endpoint — raw chunked frames visible on the wire with hex size prefixes (`44`, `45`...) added transparently by `net/http` on each `Flush()` call. Full raw output in `prototypes/01-file-upload/upload/logs/upload-raw.log`. The `0` at the end is the zero-length terminating chunk signaling end of stream.

```
curl -N --raw -X GET http://localhost:8080/upload/progress/<id>
44
event: upload-progress
data: {"progress": "80", "complete": false}

45
event: upload-progress
data: {"progress": "100", "complete": false}

45
event: upload-progress
data: {"progress": "120", "complete": false}

... (one chunk per SSE event, hex size prefix per chunk)

44
event: upload-progress
data: {"progress": "838", "complete": true}

0
```

- `44` hex = 68 bytes, `45` hex = 69 bytes — slight size variation due to progress number length
- `net/http` added the hex prefix on every `Flush()` call — the handler never wrote these
- `0` = terminating chunk, signals end of stream to the client

---

## Implementing a Raw Pool

Built in `cmd/conn-pooling/pool/pool.go` using `net.Dial` directly — no `net/http`.

### Core design: buffered channel

A buffered channel of `net.Conn` is the pool — no mutex needed, channels are already thread-safe.

```
pool chan net.Conn  // capacity = MaxConn
```

Operations:
- **Get** — `select { case conn := <-pool: return conn; default: net.Dial(...) }`
- **Return** — `select { case pool <- c: default: c.Close() }`

The `default` case in both is key:
- In `GetConn`: pool empty → dial new connection
- In `ReturnConn`: pool full → close the connection (not drop — close, or it leaks the fd and kernel resources)

**Mistake made:** Initially used `len(p.pool) == p.MaxConn` check + send, which has a race between the check and the send. A concurrent goroutine could return a connection between those two lines. The `select` with `default` is atomic — eliminates the race and the need for a mutex.

**Another mistake:** Added `sync.Mutex` on top of the channel — redundant. The channel *is* the synchronization primitive. Removed.

### Stale connection problem

When a connection sits idle in the pool, the server may close it (idle timeout → FIN). The client pool doesn't know.

**Instinct:** Check staleness on `GetConn` with a non-blocking read of 1 byte. If it returns `io.EOF`, the connection is stale — dial a new one.

**Why that doesn't work:** Reading 1 byte is destructive. If the connection is *not* stale and 1 byte of actual response data is in the recv buffer, you've consumed it. The caller gets corrupted data.

**Real approach: reactive, not proactive.** The pool hands out connections that *might* be stale. The caller handles the error — discards the bad connection (does NOT call `ReturnConn`, just `conn.Close()`), retries with a new one. The pool contract: return good connections, discard bad ones, never return a failed connection.

---

## Building the Client — Mistakes and Lessons

Built in `cmd/conn-pooling/main.go`. Evolved through several broken designs before landing on a clean one.

### CloseWrite was the hidden killer

`conn.(*net.TCPConn).CloseWrite()` sends a FIN to the server. Server's `sc.Scan()` sees EOF, the loop exits, `defer conn.Close()` fires, connection is dead. Returning this to the pool gave the next iteration a stale connection.

**`defer conn.Close()` is correct and should always be there** — it fires only when the goroutine exits (after client disconnects), which is the right time. Real-world TCP servers including `net/http` use this exact pattern. The problem was never the defer — it was `CloseWrite()` on the client side killing the connection prematurely.

**What actually ends the `for sc.Scan()` loop on the server:**
- Client calls `conn.Close()` or process exits → OS sends FIN for all open fds → server's recv buffer gets the FIN → netpoller fires EPOLLIN on the fd → parked server goroutine wakes up → `sc.Scan()` returns false → loop exits → `defer conn.Close()` fires → goroutine returns
- This is the clean path. The goroutine stays parked (consuming no OS thread) between requests, unparked only when data or a FIN arrives.

**The 10-pool / 12-iteration example:**
Say pool size is 10 and the client runs 12 parallel requests. All 12 dial new connections (pool starts empty). When returning:
- First 10 go back into the pool via `pool <- c` — their server goroutines stay alive, parked on `sc.Scan()`.
- Last 2 hit the `default` case in `ReturnConn` (pool full) → `c.Close()` is called → FIN sent → server's netpoller wakes those 2 goroutines → `sc.Scan()` returns false → goroutines exit cleanly.
- The 10 idle connections sit in the pool. Their server goroutines remain parked until the client process exits, at which point the OS closes all fds, FINs go out, and all 10 server goroutines wake up and exit.

Removing `CloseWrite()` was the fix that made pooling actually work.

### io.Copy fights the pool design

`io.Copy(os.Stdout, conn)` blocks until the connection closes. You can't use it as the reader in a pooled design — it never returns while the connection is alive, so `ReturnConn` can't be called cleanly.

`io.Copy` returning `nil` (not `io.EOF`) on clean EOF also made it impossible to distinguish "connection cleanly closed" from "error" — both result in `nil` error from `io.Copy`.

### The clean design

```
fmt.Fprintf(conn, "request\n")  // write one line
sc.Scan()                        // read exactly one line
pool.ReturnConn(conn)            // return — conn is still alive
```

No goroutines needed. Writer (`fmt.Fprintf`) is non-blocking. Reader (`sc.Scan()`) blocks until one line arrives, then returns. Clean, predictable, no races.

**Verified output:**
```
iter[0]: conn.LocalAddress(127.0.0.1:51568)
echo: [0]: hello, stay alive from
iter[1]: conn.LocalAddress(127.0.0.1:51568)
echo: [1]: hello, stay alive from
```

Same local port `51568` on both iterations — connection reused, zero new dials.

### Why local port is the proof

The kernel assigns an ephemeral local port per TCP connection. Same local port across two `GetConn` calls = same connection reused from pool. Different port = new dial happened.

---

## The 3/5 Pool Overflow Demo (3 pool size, 5 connections)

Built and verified in `cmd/conn-pooling/main.go`. Pool size = 3.

**Pool implementation (pool/pool.go at time of demo):**
```go
func New(addr string) ConnPool {
    MaxConn := 3
    return ConnPool{
        pool:    make(chan net.Conn, MaxConn),
        MaxConn: MaxConn,
        Addr:    addr,
    }
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
        c.Close() // pool full — close to avoid fd leak
    }
}
```

**Client demo (main.go at time of demo):**
```go
func main() {
    pool := pool.New(":4040")

    // grab 2 extra connections before warming pool
    conn11, _ := pool.GetConn() // dials fresh → port 53338
    conn12, _ := pool.GetConn() // dials fresh → port 53339

    // warm up pool with 3 connections
    conns := []net.Conn{}
    for i := 0; i < 3; i++ {
        conn, _ := pool.GetConn() // dials fresh → 53340, 53341, 53342
        conns = append(conns, conn)
    }
    for _, conn := range conns {
        pool.ReturnConn(conn) // pool now full: {53340, 53341, 53342}
    }

    // first loop — grabs from pool, returns after each
    for i := 0; i < 3; i++ {
        conn, _ := pool.GetConn()
        handleConn(i, conn, pool)
    }

    // second loop — same pool connections reused
    for i := 0; i < 3; i++ {
        conn, _ := pool.GetConn()
        handleConn(i, conn, pool)
    }

    // these two overflow the pool → get closed
    handleConn(3, conn11, pool)
    handleConn(4, conn12, pool)
}

func handleConn(i int, conn net.Conn, pool pool.ConnPool) {
    fmt.Printf("iter[%d]: conn.LocalAddress(%v)\n", i, conn.LocalAddr())
    fmt.Fprintf(conn, "[%d]: hello, stay alive from\n", i)
    sc := bufio.NewScanner(conn)
    sc.Scan()
    fmt.Println(sc.Text())
    pool.ReturnConn(conn)
}
```

**What happened:**
- `conn11` (port 53338) and `conn12` (port 53339) — dialed fresh, held outside the pool
- Warm-up: dial 3 connections (53340, 53341, 53342), return all to pool — pool full
- First loop of 3: grabbed 53340, 53341, 53342 from pool, used, returned — pool stays full
- Second loop of 3: grabbed same 53340, 53341, 53342 — **same ports, same server goroutines**, zero new dials
- `conn11`: `ReturnConn` → pool full → `c.Close()` → server goroutine wakes immediately → "disconnected"
- `conn12`: same — immediate disconnect
- The 3 warm connections stayed in pool until client process exited — OS sent FINs, all 3 server goroutines woke and exited

**Diagram:** `cmd/conn-pooling/tcp-connection-pooling.excalidraw` — shows 5 server goroutines (conn-1 to conn-5), pool boundary containing conn1/conn2/conn3, conn4/conn5 outside the pool (closed by `ReturnConn`).

**Server output (updated with goroutine IDs):**
```
go run server/main.go
[go-routine-0][127.0.0.1:55643]: connected
[go-routine-1][127.0.0.1:55644]: connected
[go-routine-2][127.0.0.1:55645]: connected
[go-routine-3][127.0.0.1:55646]: connected
[go-routine-4][127.0.0.1:55647]: connected
[go-routine-2][127.0.0.1:55645]: [0]: hello, stay alive from
[go-routine-3][127.0.0.1:55646]: [1]: hello, stay alive from
[go-routine-4][127.0.0.1:55647]: [2]: hello, stay alive from
[go-routine-2][127.0.0.1:55645]: [0]: hello, stay alive from   ← same goroutine, second request
[go-routine-3][127.0.0.1:55646]: [1]: hello, stay alive from   ← same goroutine, second request
[go-routine-4][127.0.0.1:55647]: [2]: hello, stay alive from   ← same goroutine, second request
[go-routine-0][127.0.0.1:55643]: [3]: hello, stay alive from
[go-routine-0][127.0.0.1:55643]: disconnected                  ← immediately, pool full
[go-routine-1][127.0.0.1:55644]: [4]: hello, stay alive from
[go-routine-1][127.0.0.1:55644]: disconnected                  ← immediately, pool full
[go-routine-4][127.0.0.1:55647]: disconnected                  ← on process exit
[go-routine-2][127.0.0.1:55645]: disconnected                  ← on process exit
[go-routine-3][127.0.0.1:55646]: disconnected                  ← on process exit
```

**Client output:**
```
go run main.go
iter[0]: conn.LocalAddress(127.0.0.1:55645)
[go-routine-2]echo: [0]: hello, stay alive from
iter[1]: conn.LocalAddress(127.0.0.1:55646)
[go-routine-3]echo: [1]: hello, stay alive from
iter[2]: conn.LocalAddress(127.0.0.1:55647)
[go-routine-4]echo: [2]: hello, stay alive from
iter[0]: conn.LocalAddress(127.0.0.1:55645)   ← same port, reused
[go-routine-2]echo: [0]: hello, stay alive from
iter[1]: conn.LocalAddress(127.0.0.1:55646)   ← same port, reused
[go-routine-3]echo: [1]: hello, stay alive from
iter[2]: conn.LocalAddress(127.0.0.1:55647)   ← same port, reused
[go-routine-4]echo: [2]: hello, stay alive from
iter[3]: conn.LocalAddress(127.0.0.1:55643)   ← conn11, overflow
[go-routine-0]echo: [3]: hello, stay alive from
iter[4]: conn.LocalAddress(127.0.0.1:55644)   ← conn12, overflow
[go-routine-1]echo: [4]: hello, stay alive from
```

**Server instrumentation — how goroutine IDs were added:**
```go
i := 0
for {
    conn, err := listener.Accept()
    // ...
    go func(conn net.Conn, gri int) {
        defer conn.Close()
        sc := bufio.NewScanner(conn)
        who := conn.RemoteAddr()

        fmt.Printf("[go-routine-%d][%s]: connected\n", gri, who)

        for sc.Scan() {
            t := sc.Text()
            fmt.Printf("[go-routine-%d][%s]: %s\n", gri, who, t)
            fmt.Fprintf(conn, "[go-routine-%d]echo: %s\n", gri, t)
        }

        fmt.Printf("[go-routine-%d][%s]: disconnected\n", gri, who)
    }(conn, i)
    i++
}
```
A simple incrementing counter `i` passed as `gri` to each goroutine. Since `Accept()` is called sequentially, goroutine IDs are assigned in connection order. The ID is captured at spawn time via the function parameter — not a closure over `i`, which would be a data race.

**Note:** The order of log lines on the server is non-deterministic — goroutines are scheduled by the Go runtime and can interleave. The goroutine ID + port combination is what ties a request to a specific connection, not the line order.

**What the goroutine IDs prove:**
- `go-routine-2` handled both iter[0] requests on port 55645 — same goroutine, same connection, two separate request-response cycles. This is connection reuse in action.
- `go-routine-0` and `go-routine-1` disconnect immediately after one request — `ReturnConn` closed them because the pool was full.
- `go-routine-2`, `3`, `4` stayed parked in `sc.Scan()` between requests, woke for each request, disconnected only when the client process exited.

---

## IdleConnTimeout — Design Brainstorm

### The problem
Idle connections sit in the pool forever. If the server has its own idle timeout, it sends a FIN. Client grabs the stale connection, writes a request, gets an error. `IdleConnTimeout` solves this by proactively evicting connections that have been idle too long.

### Design decisions

**Tracking idle time:** wrap `net.Conn` with a timestamp — `idleSince time.Time` set when `ReturnConn` is called. When `ReturnConn` is called, set `idleSince = time.Now()` and store the struct.

### Why channel → slice

A buffered channel can't be iterated or inspected without consuming. To check expiry, you need to *peek* at elements without removing them.

A **slice as a FIFO queue** works naturally:
- `ReturnConn` appends to the back — newest at the end
- `GetConn` pops from the back (newest first, still warm)
- Eviction goroutine checks the front — oldest idle connection first
- If front is not expired, nothing behind it is either → stop early

Trade-off: channel was lock-free and elegant. Slice is more complex but enables time-ordered eviction.

### Thread safety
Replacing the channel with a slice means concurrent access needs explicit synchronization — a `sync.Mutex` wrapping all reads and writes to the slice.

### The eviction goroutine
A background goroutine wakes periodically, locks the mutex, checks the front of the queue:
- `time.Since(front.idleSince) > IdleConnTimeout` → pop, `conn.Close()`, repeat
- Otherwise → stop (rest are newer)

**How often does the eviction goroutine wake up?**

`IdleConnTimeout / 2` is the right answer.

Reasoning: worst case, a connection becomes idle just *after* a wakeup. The next wakeup is `IdleConnTimeout / 2` later — at which point the connection has been idle for `IdleConnTimeout / 2`. One more interval passes, eviction fires. Maximum overshoot: one interval = `IdleConnTimeout / 2`.

Every 1 second is wasteful — for a 90s timeout that's 90 wakeups per eviction. Every 60s with a 90s timeout means a connection could live up to 150s. `IdleConnTimeout / 2` bounds the worst case cleanly.

Go's `net/http` uses exactly this interval internally.

**Eviction in `ReturnConn` is opportunistic** — the background goroutine handles periodic cleanup. The eviction check in `ReturnConn` is an optimization: when the pool is full, swap a stale idle connection for the fresh returning one instead of closing the returner. Without it, the returner gets closed even if older pool connections are already stale. Minor edge case, not strictly necessary.

**`GetConn` must release lock before dialing** — `net.Dial` can block for hundreds of ms. Holding the mutex during dial would block all other goroutines from getting or returning connections.

### Final implementation (`pool/pool.go`)

```go
type ConnPool struct {
    mu    sync.Mutex
    pool  []struct {
        idleSince time.Time
        conn      net.Conn
    }
    MaxConn         int
    IdleConnTimeout time.Duration
    Addr            string
}

func New(addr string) *ConnPool {
    p := &ConnPool{
        MaxConn:         3,
        Addr:            addr,
        IdleConnTimeout: 60 * time.Second,
    }

    go func() {
        for {
            time.Sleep(p.IdleConnTimeout / 2)
            p.mu.Lock()
            if len(p.pool) == 0 {
                p.mu.Unlock()
                continue
            }
            // evict all stale connections from the front
            for len(p.pool) > 0 && time.Since(p.pool[0].idleSince) >= p.IdleConnTimeout {
                p.pool[0].conn.Close()
                p.pool = p.pool[1:]
            }
            p.mu.Unlock()
        }
    }()
    return p
}

func (p *ConnPool) GetConn() (net.Conn, error) {
    p.mu.Lock()
    if len(p.pool) == 0 {
        p.mu.Unlock()
        return net.Dial("tcp", p.Addr) // dial outside the lock
    }
    conn := p.pool[0].conn
    p.pool = p.pool[1:]
    p.mu.Unlock()
    return conn, nil
}

func (p *ConnPool) ReturnConn(c net.Conn) {
    p.mu.Lock()
    defer p.mu.Unlock()

    if len(p.pool) == p.MaxConn {
        // pool full — evict oldest if stale, otherwise close incoming
        if time.Since(p.pool[0].idleSince) >= p.IdleConnTimeout {
            p.pool[0].conn.Close()
            p.pool = append(p.pool[1:], struct {
                idleSince time.Time
                conn      net.Conn
            }{conn: c, idleSince: time.Now()})
        } else {
            c.Close()
        }
        return
    }

    p.pool = append(p.pool, struct {
        idleSince time.Time
        conn      net.Conn
    }{idleSince: time.Now(), conn: c})
}
```

---

## IdleConnTimeout — Eviction Test

### Server instrumentation update

Switched `fmt.Printf` → `log.Printf` throughout the server. `log` adds timestamps automatically — no manual `time.Now()` calls needed. This made it possible to read the exact moment each goroutine disconnected from the log output.

```go
log.Printf("[go-routine-%d][%s]: connected\n", gri, who)

for sc.Scan() {
    t := sc.Text()
    log.Printf("[go-routine-%d][%s]: %s\n", gri, who, t)
    fmt.Fprintf(conn, "[go-routine-%d]echo: %s\n", gri, t)
}

log.Printf("[go-routine-%d][%s]: disconnected\n", gri, who)
```

### Test setup

`IdleConnTimeout` lowered to `5` seconds in `New()`. Client ran the same 3-pool / 5-connection demo, then process exited. Server kept running.

### What happened

**Server output:**
```
2026/05/10 00:31:59 [go-routine-0][127.0.0.1:57048]: connected
2026/05/10 00:31:59 [go-routine-1][127.0.0.1:57049]: connected
2026/05/10 00:31:59 [go-routine-2][127.0.0.1:57050]: connected
2026/05/10 00:31:59 [go-routine-3][127.0.0.1:57051]: connected
2026/05/10 00:31:59 [go-routine-4][127.0.0.1:57052]: connected
2026/05/10 00:31:59 [go-routine-2][127.0.0.1:57050]: [0]: hello, stay alive from
2026/05/10 00:31:59 [go-routine-3][127.0.0.1:57051]: [1]: hello, stay alive from
2026/05/10 00:31:59 [go-routine-4][127.0.0.1:57052]: [2]: hello, stay alive from
2026/05/10 00:31:59 [go-routine-2][127.0.0.1:57050]: [0]: hello, stay alive from
2026/05/10 00:31:59 [go-routine-3][127.0.0.1:57051]: [1]: hello, stay alive from
2026/05/10 00:31:59 [go-routine-4][127.0.0.1:57052]: [2]: hello, stay alive from
2026/05/10 00:31:59 [go-routine-0][127.0.0.1:57048]: [3]: hello, stay alive from
2026/05/10 00:31:59 [go-routine-0][127.0.0.1:57048]: disconnected   ← overflow, immediate
2026/05/10 00:31:59 [go-routine-1][127.0.0.1:57049]: [4]: hello, stay alive from
2026/05/10 00:31:59 [go-routine-1][127.0.0.1:57049]: disconnected   ← overflow, immediate
2026/05/10 00:32:04 [go-routine-2][127.0.0.1:57050]: disconnected   ← evicted, 5s later
2026/05/10 00:32:04 [go-routine-4][127.0.0.1:57052]: disconnected   ← evicted, 5s later
2026/05/10 00:32:04 [go-routine-3][127.0.0.1:57051]: disconnected   ← evicted, 5s later
```

**Client output:**
```
iter[0]: conn.LocalAddress(127.0.0.1:57050)
[go-routine-2]echo: [0]: hello, stay alive from
iter[1]: conn.LocalAddress(127.0.0.1:57051)
[go-routine-3]echo: [1]: hello, stay alive from
iter[2]: conn.LocalAddress(127.0.0.1:57052)
[go-routine-4]echo: [2]: hello, stay alive from
iter[0]: conn.LocalAddress(127.0.0.1:57050)   ← same port, reused
[go-routine-2]echo: [0]: hello, stay alive from
iter[1]: conn.LocalAddress(127.0.0.1:57051)   ← same port, reused
[go-routine-3]echo: [1]: hello, stay alive from
iter[2]: conn.LocalAddress(127.0.0.1:57052)   ← same port, reused
[go-routine-4]echo: [2]: hello, stay alive from
iter[3]: conn.LocalAddress(127.0.0.1:57048)   ← overflow
[go-routine-0]echo: [3]: hello, stay alive from
iter[4]: conn.LocalAddress(127.0.0.1:57049)   ← overflow
[go-routine-1]echo: [4]: hello, stay alive from
2026/05/10 00:32:04 [pool-idle-conn-eviction] closing conn: 127.0.0.1:57050
2026/05/10 00:32:04 [pool-idle-conn-eviction] closing conn: 127.0.0.1:57051
2026/05/10 00:32:04 [pool-idle-conn-eviction] closing conn: 127.0.0.1:57052
```

### What the timestamps prove

- `00:31:59` — all requests completed, 3 connections returned to pool, `idleSince` set
- `00:31:59` — overflow connections (57048, 57049) closed immediately by `ReturnConn` — server goroutines 0 and 1 disconnected right away
- `00:32:04` — exactly 5 seconds later, eviction goroutine fired — client closed all 3 pool connections, FINs sent to server
- `00:32:04` — server go-routine-2, 3, 4 woke from `sc.Scan()`, printed "disconnected" — without the client process ever exiting

This is the critical distinction from the previous demo: the server goroutines were freed by the **eviction goroutine**, not by process shutdown. The client had already exited but left long-lived connections in the kernel. The eviction goroutine is what cleans them up proactively.

---

## MaxConnsPerHost — Design and Implementation

### The distinction

- `MaxIdleConnsPerHost` — caps idle connections sitting in the pool. Purpose: avoid stale connections accumulating when traffic is low.
- `MaxConnsPerHost` — caps **total** connections (idle + in-use). Purpose: protect the server from being overwhelmed; set higher than `MaxIdleConnsPerHost` to absorb burst traffic.

`MaxConnsPerHost` ≥ `MaxIdleConnsPerHost` always. The difference is the burst headroom — extra connections created under load, then closed on `ReturnConn` when the pool is already full, naturally draining back to the idle cap.

### The semaphore pattern

`MaxConnsPerHost` is implemented as a **pre-filled buffered channel** (`chan struct{}`):

```go
maxConnsPerHost := make(chan struct{}, MaxConnsPerHost)
for i := 0; i < MaxConnsPerHost; i++ {
    maxConnsPerHost <- struct{}{}
}
```

- **Acquire** (dial new connection): `<-p.MaxConnsPerHost` — receive a token. Blocks when channel is empty = all `MaxConnsPerHost` connections are live.
- **Release** (close a connection): `p.MaxConnsPerHost <- struct{}{}` — return a token.

Token accounting:
- Dial new connection: consume token ✓
- Get from pool: no change — slot was counted at dial time ✓
- Return to pool: no change — connection still exists ✓
- Close (pool full, `ReturnConn`): release token ✓
- Close (eviction goroutine): release token ✓

Why `chan struct{}` not `chan int`: `struct{}` is zero-size — no allocation per token. Conventional Go semaphore type.

### What happens when at the limit

`GetConn` finds the pool empty and blocks on `<-p.MaxConnsPerHost`. It parks the goroutine — no spin, no CPU burn. When any `ReturnConn` or eviction closes a connection and sends a token back, `GetConn` unblocks and dials.

This is the same behaviour as `net/http` — callers wait, never error, when `MaxConnsPerHost` is exhausted.

### Logging blocked callers

Wanted to log when `GetConn` actually blocks. First attempt — busy-wait spin:

```go
for {
    select {
    case <-p.MaxConnsPerHost:
    default:
        log.Printf("blocking for MaxConnsPerHost")
        continue
    }
}
```

Wrong — `default` means it never parks. Burns 100% CPU.

Second attempt — non-blocking check then blocking receive:

```go
select {
case <-p.MaxConnsPerHost:
default:
    log.Printf("blocking for MaxConnsPerHost")
    <-p.MaxConnsPerHost
}
```

Works. But there's a subtle TOCTOU (Time-Of-Check-To-Time-Of-Use) in the logging:
- **Check**: non-blocking `select` asks "is a token available?" → yes, hits `default`
- **Act**: `<-p.MaxConnsPerHost` takes the token
- **Between the two**: another goroutine could grab the last token

By the time the act happens, what was checked is no longer true — the log says "blocking" but the goroutine may not actually block (or blocks only for nanoseconds). Cosmetically inaccurate but negligible in practice. No way to eliminate without a lock around check+receive, which defeats the purpose.

Simplest alternative — always log before the receive:

```go
log.Printf("blocking for MaxConnsPerHost")
<-p.MaxConnsPerHost
```

Logs even when a token is immediately available. Tradeoff: simpler, noisier. Final choice: the select approach — only logs on the slow path, good enough for observability.

---

## MaxConnsPerHost — Live Demo (10 goroutines, limit 6)

### pool/pool.go at this point

```go
type ConnPool struct {
    mu   sync.Mutex
    pool []struct {
        idleSince time.Time
        conn      net.Conn
    }

    // for avoiding many stale connections in pool
    MaxIdleConnsPerHost int

    // (semaphore) for handling burst requests and also to avoid overwhelming server
    MaxConnsPerHost chan struct{}

    IdleConnTimeout time.Duration
    Addr            string
}

func New(addr string) *ConnPool {
    MaxConnsPerHost := 6
    MaxIdleConnsPerHost := 3
    IdleConnTimeout := 5 // seconds, low for testing

    maxConnsPerHost := make(chan struct{}, MaxConnsPerHost)
    for i := 0; i < MaxConnsPerHost; i++ {
        maxConnsPerHost <- struct{}{}
    }

    pool := &ConnPool{
        pool:                []struct{ idleSince time.Time; conn net.Conn }{},
        MaxConnsPerHost:     maxConnsPerHost,
        MaxIdleConnsPerHost: MaxIdleConnsPerHost,
        Addr:                addr,
        IdleConnTimeout:     time.Second * time.Duration(IdleConnTimeout),
    }

    go evictIdleTimedoutConns(pool)
    return pool
}

func evictIdleTimedoutConns(p *ConnPool) {
    for {
        time.Sleep(p.IdleConnTimeout / 2)
        p.mu.Lock()

        if len(p.pool) == 0 {
            p.mu.Unlock()
            continue
        }

        for {
            if time.Since(p.pool[0].idleSince) >= p.IdleConnTimeout {
                oldConn := p.pool[0].conn
                log.Printf("[pool-idle-conn-eviction] closing conn: %v", oldConn.LocalAddr())
                oldConn.Close()
                p.MaxConnsPerHost <- struct{}{} // release semaphore slot
                if len(p.pool) == 1 {
                    p.pool = []struct{ idleSince time.Time; conn net.Conn }{}
                    break
                }
                p.pool = p.pool[1:]
            } else {
                break
            }
        }

        p.mu.Unlock()
    }
}

func (p *ConnPool) GetConn() (net.Conn, error) {
    p.mu.Lock()
    if len(p.pool) == 0 {
        p.mu.Unlock()
        // non-blocking check first to log before actually blocking
        select {
        case <-p.MaxConnsPerHost:
        default:
            log.Printf("blocking for MaxConnsPerHost")
            <-p.MaxConnsPerHost // park goroutine until a slot is released
        }
        return net.Dial("tcp", p.Addr)
    }

    defer p.mu.Unlock()
    conn := p.pool[0].conn
    p.pool = p.pool[1:]
    return conn, nil
}

func (p *ConnPool) ReturnConn(c net.Conn) {
    p.mu.Lock()
    defer p.mu.Unlock()

    if len(p.pool) == p.MaxIdleConnsPerHost {
        // opportunistic idle-conn-eviction: swap stale idle for fresh returning conn
        if time.Since(p.pool[0].idleSince) >= p.IdleConnTimeout {
            p.pool[0].conn.Close()
            p.MaxConnsPerHost <- struct{}{} // release slot for closed conn
            newPool := append([]struct{ idleSince time.Time; conn net.Conn }{}, p.pool[1:]...)
            newPool = append(newPool, struct{ idleSince time.Time; conn net.Conn }{conn: c, idleSince: time.Now()})
            p.pool = newPool
            return
        }
        // pool full, not stale — close returning conn
        c.Close()
        p.MaxConnsPerHost <- struct{}{} // release slot
        return
    }

    p.pool = append(p.pool, struct {
        idleSince time.Time
        conn      net.Conn
    }{idleSince: time.Now(), conn: c})
    // no semaphore change — connection still exists, just moved to idle
}
```

### Setup

`MaxConnsPerHost = 6`, `MaxIdleConnsPerHost = 3`. 10 goroutines launched simultaneously, each calling `GetConn`, sleeping 2 seconds, then `ReturnConn`. A `sync.WaitGroup` keeps `main` alive until all finish.

```go
wg := sync.WaitGroup{}
for i := 0; i < 10; i++ {
    wg.Add(1)
    go func(i int) {
        defer wg.Done()
        conn, _ := pool.GetConn()
        fmt.Printf("iter[%d]: conn.LocalAddress(%v)\n", i, conn.LocalAddr())
        fmt.Fprintf(conn, "[%d]: hello, stay alive from\n", i)
        time.Sleep(time.Second * 2)
        sc := bufio.NewScanner(conn)
        sc.Scan()
        fmt.Println(sc.Text())
        pool.ReturnConn(conn)
    }(i)
}
wg.Wait()
```

### Client output — without logging race condition

All 4 "blocking" logs appear before any "using" — goroutines raced to the semaphore and lost before any winner printed.

```
2026/05/10 02:26:36 [go-routine-6] blocking for MaxConnsPerHost   ← all 4 blocked upfront
2026/05/10 02:26:36 [go-routine-7] blocking for MaxConnsPerHost
2026/05/10 02:26:36 [go-routine-8] blocking for MaxConnsPerHost
2026/05/10 02:26:36 [go-routine-4] blocking for MaxConnsPerHost
2026/05/10 02:26:36 [go-routine-1] using conn.LocalAddress(127.0.0.1:58963)   ← 6 dial immediately
2026/05/10 02:26:36 [go-routine-3] using conn.LocalAddress(127.0.0.1:58964)
2026/05/10 02:26:36 [go-routine-2] using conn.LocalAddress(127.0.0.1:58966)
2026/05/10 02:26:36 [go-routine-5] using conn.LocalAddress(127.0.0.1:58967)
2026/05/10 02:26:36 [go-routine-0] using conn.LocalAddress(127.0.0.1:58965)
2026/05/10 02:26:36 [go-routine-9] using conn.LocalAddress(127.0.0.1:58968)
[response-from-server]:[go-routine-5]echo: [9]: hi
[response-from-server]:[go-routine-1]echo: [2]: hi
[response-from-server]:[go-routine-2]echo: [5]: hi
[response-from-server]:[go-routine-0]echo: [1]: hi
[response-from-server]:[go-routine-4]echo: [3]: hi
[response-from-server]:[go-routine-3]echo: [0]: hi
2026/05/10 02:26:38 [go-routine-6] using conn.LocalAddress(127.0.0.1:58971)   ← 3 unblock, 2s later
2026/05/10 02:26:38 [go-routine-8] using conn.LocalAddress(127.0.0.1:58970)
2026/05/10 02:26:38 [go-routine-7] using conn.LocalAddress(127.0.0.1:58969)
[response-from-server]:[go-routine-8]echo: [8]: hi
[response-from-server]:[go-routine-7]echo: [7]: hi
[response-from-server]:[go-routine-6]echo: [6]: hi
2026/05/10 02:26:40 [go-routine-4] using conn.LocalAddress(127.0.0.1:58973)   ← last blocked goroutine
[response-from-server]:[go-routine-9]echo: [4]: hi
```

### Client output — interleaved scheduling

go-routine-2 logs "blocking" but go-routine-4 has already printed "using" on the very next line. This *looks* like the logging race but cannot be confirmed from logs alone — go-routine-4, 7, 0, 5, 1, 9 could simply have been scheduled before go-routine-2, 8, 3, 6 and grabbed all 6 tokens first. The logging race is a theoretical possibility in the design — a TOCTOU (Time-Of-Check-To-Time-Of-Use): the non-blocking `select` checks "is a token available?" and finds yes, but before the actual `<-p.MaxConnsPerHost` executes, another goroutine grabs that token. By the time the act happens, what was checked is no longer true. These logs don't prove it was triggered.

```
2026/05/10 02:25:51 [go-routine-2] blocking for MaxConnsPerHost   ← blocked
2026/05/10 02:25:51 [go-routine-4] using conn.LocalAddress(127.0.0.1:58944)   ← already using!
2026/05/10 02:25:51 [go-routine-8] blocking for MaxConnsPerHost
2026/05/10 02:25:51 [go-routine-3] blocking for MaxConnsPerHost
2026/05/10 02:25:51 [go-routine-6] blocking for MaxConnsPerHost
2026/05/10 02:25:51 [go-routine-7] using conn.LocalAddress(127.0.0.1:58943)
2026/05/10 02:25:51 [go-routine-0] using conn.LocalAddress(127.0.0.1:58940)
2026/05/10 02:25:51 [go-routine-5] using conn.LocalAddress(127.0.0.1:58942)
2026/05/10 02:25:51 [go-routine-1] using conn.LocalAddress(127.0.0.1:58939)
2026/05/10 02:25:51 [go-routine-9] using conn.LocalAddress(127.0.0.1:58941)
[response-from-server]:[go-routine-4]echo: [7]: hi
[response-from-server]:[go-routine-3]echo: [5]: hi
[response-from-server]:[go-routine-0]echo: [0]: hi
[response-from-server]:[go-routine-2]echo: [9]: hi
[response-from-server]:[go-routine-5]echo: [4]: hi
[response-from-server]:[go-routine-1]echo: [1]: hi
2026/05/10 02:25:53 [go-routine-2] using conn.LocalAddress(127.0.0.1:58945)   ← 3 unblock, 2s later
2026/05/10 02:25:53 [go-routine-8] using conn.LocalAddress(127.0.0.1:58946)
2026/05/10 02:25:53 [go-routine-6] using conn.LocalAddress(127.0.0.1:58947)
[response-from-server]:[go-routine-7]echo: [2]: hi
[response-from-server]:[go-routine-8]echo: [6]: hi
[response-from-server]:[go-routine-6]echo: [8]: hi
2026/05/10 02:25:55 [go-routine-3] using conn.LocalAddress(127.0.0.1:58948)   ← last blocked goroutine
[response-from-server]:[go-routine-9]echo: [3]: hi
```

### Server output — clean run (fresh server, go-routines 0–9)

```
2026/05/10 02:26:36 [go-routine-0][127.0.0.1:58963]: connected
2026/05/10 02:26:36 [go-routine-1][127.0.0.1:58966]: connected
2026/05/10 02:26:36 [go-routine-2][127.0.0.1:58967]: connected
2026/05/10 02:26:36 [go-routine-3][127.0.0.1:58965]: connected
2026/05/10 02:26:36 [go-routine-4][127.0.0.1:58964]: connected
2026/05/10 02:26:36 [go-routine-5][127.0.0.1:58968]: connected   ← 6 connections, semaphore full
... (requests handled) ...
2026/05/10 02:26:38 [go-routine-0][127.0.0.1:58963]: disconnected
2026/05/10 02:26:38 [go-routine-3][127.0.0.1:58965]: disconnected
2026/05/10 02:26:38 [go-routine-4][127.0.0.1:58964]: disconnected   ← 3 closed → 3 tokens released
2026/05/10 02:26:38 [go-routine-6][127.0.0.1:58971]: connected       ← go-routine-6 unblocked
2026/05/10 02:26:38 [go-routine-7][127.0.0.1:58969]: connected       ← go-routine-7 unblocked
2026/05/10 02:26:38 [go-routine-8][127.0.0.1:58970]: connected       ← go-routine-8 unblocked
2026/05/10 02:26:40 [go-routine-6][127.0.0.1:58971]: disconnected
2026/05/10 02:26:40 [go-routine-7][127.0.0.1:58969]: disconnected
2026/05/10 02:26:40 [go-routine-8][127.0.0.1:58970]: disconnected
2026/05/10 02:26:40 [go-routine-9][127.0.0.1:58973]: connected       ← go-routine-4 (last blocked) unblocked
2026/05/10 02:26:42 [go-routine-9][127.0.0.1:58973]: disconnected
2026/05/10 02:26:42 [go-routine-1][127.0.0.1:58966]: disconnected   ← pool connections, on process exit
2026/05/10 02:26:42 [go-routine-2][127.0.0.1:58967]: disconnected
2026/05/10 02:26:42 [go-routine-5][127.0.0.1:58968]: disconnected
```

### Server output — interleaved scheduling run (fresh server, go-routines 0–9)

```
2026/05/10 02:25:51 [go-routine-0][127.0.0.1:58940]: connected
2026/05/10 02:25:51 [go-routine-1][127.0.0.1:58939]: connected
2026/05/10 02:25:51 [go-routine-2][127.0.0.1:58941]: connected
2026/05/10 02:25:51 [go-routine-3][127.0.0.1:58942]: connected
2026/05/10 02:25:51 [go-routine-4][127.0.0.1:58943]: connected
2026/05/10 02:25:51 [go-routine-5][127.0.0.1:58944]: connected   ← 6 connections, semaphore full
... (requests handled) ...
2026/05/10 02:25:53 [go-routine-1][127.0.0.1:58939]: disconnected
2026/05/10 02:25:53 [go-routine-2][127.0.0.1:58941]: disconnected
2026/05/10 02:25:53 [go-routine-5][127.0.0.1:58944]: disconnected   ← 3 closed → 3 tokens released
2026/05/10 02:25:53 [go-routine-6][127.0.0.1:58946]: connected       ← 3 blocked goroutines unblocked
2026/05/10 02:25:53 [go-routine-7][127.0.0.1:58945]: connected
2026/05/10 02:25:53 [go-routine-8][127.0.0.1:58947]: connected
2026/05/10 02:25:55 [go-routine-6][127.0.0.1:58946]: disconnected
2026/05/10 02:25:55 [go-routine-7][127.0.0.1:58945]: disconnected
2026/05/10 02:25:55 [go-routine-8][127.0.0.1:58947]: disconnected
2026/05/10 02:25:55 [go-routine-9][127.0.0.1:58948]: connected       ← go-routine-3 (last blocked) unblocked
2026/05/10 02:25:57 [go-routine-9][127.0.0.1:58948]: disconnected
2026/05/10 02:25:57 [go-routine-0][127.0.0.1:58940]: disconnected   ← pool connections, on process exit
2026/05/10 02:25:57 [go-routine-3][127.0.0.1:58942]: disconnected
2026/05/10 02:25:57 [go-routine-4][127.0.0.1:58943]: disconnected
```

### Reading the logs

**`02:26:36` — launch (without race):**
- go-routine-6, 7, 8, 4 log "blocking" before any "using" — all 4 lost the semaphore race before any winner printed
- go-routine-1, 3, 2, 5, 0, 9 dial immediately — 6 tokens consumed, semaphore empty

**`02:26:38` — wave 1 unblock (2 seconds later):**
- First 6 goroutines finish `time.Sleep(2s)` and call `ReturnConn`
- `MaxIdleConnsPerHost = 3` — pool accepts 3. Tokens stay consumed for those 3 — still exist as idle.
- The other 3 are closed (pool full) → 3 tokens released → go-routine-6, 7, 8 unblock — new ports, fresh dials

**`02:26:40` — wave 2 unblock (2 more seconds later):**
- Wave 1 goroutines return. Again: some to pool (tokens stay), some closed (token released) — go-routine-4 finally unblocks.
- New port 58973.

**Server side (without race):**
- 6 connections at `02:26:36`, exactly matching `MaxConnsPerHost`
- 3 disconnect at `02:26:38` (pool full → closed by `ReturnConn`) — immediately followed by 3 new connects
- Pattern repeats. Last goroutine connects at `02:26:40`.

**Interleaved scheduling (`02:25:51`):**
- go-routine-2 logs "blocking" but go-routine-4 has already printed "using" — this *looks* like the logging race but cannot be confirmed. go-routine-4 may simply have been scheduled first and grabbed the token legitimately. Correctness unaffected either way — go-routine-2 eventually got a connection at `02:25:53`.

### The wave pattern — key insight

Expected: all 4 blocked goroutines unblock together when the first 6 return.

**What actually happened:** 3 unblocked at wave 1, then 1 more at wave 2. Why?

When the first 6 return:
- `MaxIdleConnsPerHost = 3` — pool accepts 3 connections. Their semaphore tokens **stay consumed** — those connections still exist (idle in pool).
- The other 3 are closed (pool full) — `ReturnConn` calls `c.Close()` + `p.MaxConnsPerHost <- struct{}{}`. These 3 token releases unblock 3 of the 4 waiting goroutines.

**Semaphore tokens are only released when a connection is destroyed, not when it's returned to the pool.** A connection sitting idle in the pool still counts toward `MaxConnsPerHost`.

The 4th blocked goroutine had to wait until wave 2's `ReturnConn` closed another connection.

This is the subtle interaction between `MaxIdleConnsPerHost` and `MaxConnsPerHost`:
- `MaxIdleConnsPerHost` controls how many connections survive a return
- `MaxConnsPerHost` controls the hard ceiling
- Under burst: connections pile up → pool fills → excess get closed → semaphore releases → waiters unblock in waves, not all at once

---

## Dedicated-Host vs Shared-Host Pool

### What we have now — dedicated-host

The current `ConnPool` has `Addr string` hardcoded at construction time. Every `GetConn` dials the same address. Every idle connection in the pool is a connection to that one server.

This is a **dedicated-host pool** — one pool instance per target host. The caller decides the topology: want to talk to three hosts? Create three `ConnPool` instances.

All knobs (`MaxIdleConnsPerHost`, `MaxConnsPerHost`, `IdleConnTimeout`) scope naturally to that one host. `MaxConnsPerHost` is a single semaphore because there's only one server to protect.

### What changes for a shared-host pool

A single `ConnPool` that can talk to multiple hosts needs to group connections by host. The key insight: you can't hand a connection to `api.github.com` to a caller that asked for `api.stripe.com`. So the flat slice becomes a map:

```
map[addr string] → []struct{ idleSince time.Time; conn net.Conn }
```

`MaxConnsPerHost` is still **per-host** — it protects each individual server from being overwhelmed. A global cap here wouldn't help: the risk is per-server overload, not total connection count. So it becomes:

```
map[addr string] → chan struct{}  // semaphore per host
```

### Two scopes for idle connection limits

For idle connections, there are now two different concerns:

- **`MaxIdleConnsPerHost`** — per-host, same as before. Avoids stale connections accumulating on any one host.
- **`MaxIdleConns`** — global ceiling across all hosts. Prevents a client talking to 50 hosts from holding 50 × `MaxIdleConnsPerHost` idle connections, burning fds and RAM even on rarely-called services.

These are not the same concern. One protects per-server stale accumulation. The other protects the client process itself from resource exhaustion across a large number of hosts.

`ReturnConn` on a shared pool therefore has **two checks** before accepting a connection back:
1. Has this host's idle count hit `MaxIdleConnsPerHost`?
2. Has the global idle count hit `MaxIdleConns`?

Fail either check → close the returner, release the semaphore token for that host.

### Dedicated vs shared — the tradeoff

| | Dedicated-host | Shared-host |
|---|---|---|
| Knob scoping | Natural — all knobs for one host | Needs two maps + global idle cap |
| Tuning | Per-service, precise | One set of knobs for everything |
| Idle waste | Predictable | Need `MaxIdleConns` to bound total |
| Complexity | Simple | More moving parts |

**`net/http` uses shared-host** — one `http.Transport` manages all hosts, keyed by `addr`. It has both `MaxIdleConnsPerHost` and `MaxIdleConns`.

**Microservice pattern** — dedicated `http.Client` per high-traffic downstream service (precise tuning, simple); shared `http.Client` for low-traffic services (less waste, needs both idle knobs set).

This is exactly the same trade-off discovered in the Airflow context: `POOL_SIZE` + `MAX_OVERFLOW` per database, but a global connection limit at the process level to protect the container.

---

## Shared-Host Pool — Implementation

Built in `cmd/conn-pooling/pool/shared_host_pool.go`.

### Struct

```go
type SharedConnPool struct {
    mu   sync.Mutex
    pool map[string][]struct {
        idleSince time.Time
        conn      net.Conn
    }

    MaxIdleConnsPerHost int
    MaxIdleConns        int
    IdleConns           chan struct{} // global idle semaphore (pre-filled)
    IdleConnTimeout     time.Duration

    MaxConns        map[string]chan struct{} // per-host semaphore map
    MaxConnsPerHost int
}
```

Two maps instead of the dedicated pool's single slice:
- `pool` — keyed by addr, value is FIFO slice of idle connections
- `MaxConns` — keyed by addr, value is pre-filled semaphore per host

`IdleConns` is the global idle semaphore — pre-filled with `MaxIdleConns` tokens.

### GetConn

```go
func (p *SharedConnPool) GetConn(addr string) (net.Conn, error) {
    p.mu.Lock()
    pool, ok := p.pool[addr]
    if !ok || len(pool) == 0 {
        // check/init semaphore for this host under the lock
        mc, ok := p.MaxConns[addr]
        if !ok {
            p.MaxConns[addr] = make(chan struct{}, p.MaxConnsPerHost)
            for i := 0; i < p.MaxConnsPerHost; i++ {
                p.MaxConns[addr] <- struct{}{}
            }
            mc = p.MaxConns[addr]
        }
        p.mu.Unlock()
        <-mc                          // acquire per-host slot (may block)
        return net.Dial("tcp", addr)
    }

    conn := pool[0].conn
    p.pool[addr] = p.pool[addr][1:]
    p.mu.Unlock()

    p.IdleConns <- struct{}{} // release one global idle slot (never blocks — see invariant)
    return conn, nil
}
```

**Semaphore init under the lock** — the check-and-initialize for `MaxConns[addr]` happens while holding the mutex. Releasing first would create a TOCTOU: two goroutines could both see `ok = false` and both initialize the semaphore, doubling the tokens.

**Dial outside the lock** — `net.Dial` can block for hundreds of ms. Lock released before acquiring the semaphore and dialing.

**`IdleConns` release on pool-hit** — when a connection leaves the pool, its global idle slot is freed. This send never blocks because of the invariant below.

### The IdleConns invariant

```
tokens_in_IdleConns + connections_in_pool = MaxIdleConns
```

- `ReturnConn` (success path) consumes a token (`<-p.IdleConns`) before adding to pool
- `GetConn` pool-hit releases a token (`p.IdleConns <- struct{}{}`) after removing from pool
- These are symmetric — the count is always conserved

**Why the release in `GetConn` never blocks:** `GetConn` only reaches the pool-hit path if there's a connection in the pool. That connection was added by `ReturnConn` which consumed a token. So `tokens_in_IdleConns <= MaxIdleConns - 1` — the channel has room. Send always succeeds.

**Why `p.IdleConns <- struct{}{}` is outside the lock:** the invariant guarantees it won't block, but holding a mutex across a channel send is a bad habit — if the invariant ever broke, it would deadlock instead of just pausing. Moved outside the lock as a safety convention.

### ReturnConn

```go
func (p *SharedConnPool) ReturnConn(conn net.Conn) {
    addr := conn.RemoteAddr().String()
    p.mu.Lock()

    pool, ok := p.pool[addr]
    if ok && len(pool) == p.MaxIdleConnsPerHost {
        // per-host cap hit
        conn.Close()
        p.MaxConns[addr] <- struct{}{} // release per-host slot
        p.mu.Unlock()
        return
    }

    defer p.mu.Unlock()

    select {
    case <-p.IdleConns: // consume one global idle slot
        pool = append(pool, struct {
            idleSince time.Time
            conn      net.Conn
        }{idleSince: time.Now(), conn: conn})
        p.pool[addr] = pool
    default:
        // global cap hit
        conn.Close()
        p.MaxConns[addr] <- struct{}{} // release per-host slot
    }
}
```

Two rejection paths before accepting a connection into the pool:
1. **Per-host cap** — `len(pool) == MaxIdleConnsPerHost` → close, release per-host token
2. **Global cap** — `<-p.IdleConns` hits `default` (channel empty, all global slots consumed) → close, release per-host token

**API contract:** `ReturnConn` must only be called with connections obtained from `GetConn` on the same pool. If an externally created connection is passed, `p.MaxConns[addr]` may not exist. By the chain — dial initializes `MaxConns[addr]` → connection returned can only be one that was dialed — the nil map case cannot occur in correct usage.

### Eviction goroutine

```go
func evictSharedIdleTimedoutConns(pool *SharedConnPool) {
    for {
        time.Sleep(pool.IdleConnTimeout / 2)

        timedOutConns := []net.Conn{}
        pool.mu.Lock()

        for addr := range pool.pool {
            for {
                if len(pool.pool[addr]) == 0 {
                    break
                }
                c := pool.pool[addr][0]
                if time.Since(c.idleSince) > pool.IdleConnTimeout {
                    pool.MaxConns[addr] <- struct{}{} // release per-host slot
                    pool.pool[addr] = pool.pool[addr][1:]
                    pool.IdleConns <- struct{}{}       // release global slot
                    timedOutConns = append(timedOutConns, c.conn)
                } else {
                    break
                }
            }
        }

        pool.mu.Unlock()

        for _, v := range timedOutConns {
            v.Close() // closed outside the lock
        }
    }
}
```

**Collect then close** — stale connections are identified and trimmed from the slice under the lock. The `conn.Close()` syscalls happen after `Unlock()`. This keeps the critical section tight — no syscalls while blocking `GetConn` and `ReturnConn` across all hosts.

**`timedOutConns` declared inside the loop** — resets each iteration. No carry-over from previous eviction runs.

**Early break** — connections are FIFO by `idleSince`. If the oldest is not stale, nothing behind it is. Stop immediately.

---

## Connection Pooling + Framing Protocol — The Database Analogy

The echo server's `for sc.Scan()` loop is a simplification. In a real system — a database — the same loop does something far more interesting.

### What the server's loop actually does

```
for sc.Scan() {
    // 1. read bytes off the connection
    // 2. use framing protocol to detect "end of query"
    // 3. execute the query
    // 4. write response back to the connection (e.g. table rows)
    // 5. write a sentinel frame: "response complete"
    // 6. loop — wait for next query on the same connection
}
```

The connection stays alive. The server never closes it after handling a query — that would kill pooling. Instead it relies on `IdleConnTimeout` to clean up connections the client has abandoned.

### The framing protocol does two jobs

**Client → Server (query delimiting):**
The server's `sc.Scan()` needs to know where a query ends. A single `\n` works for simple line protocols. Real databases use length-prefixed frames or special terminator bytes — the server reads until it sees the frame boundary, then executes.

**Server → Client (response delimiting):**
The client needs to know when it has consumed the *entire* response before returning the connection to the pool. Return the connection mid-response — say after reading half the rows of a SELECT — and the next caller gets a connection with leftover response bytes in the recv buffer. Corrupted data.

The server signals end-of-response with a **sentinel frame** — a special marker after all data rows saying "this response is complete."

### Why not close the connection after writing?

First instinct: server writes response, closes connection, client sees EOF, knows it's done. Clean.

But closing the connection after every response destroys pooling entirely — every query pays the TCP handshake cost again. The whole point of pooling is to keep the connection alive across multiple request-response cycles.

### Why not Content-Length upfront?

HTTP solves this with `Content-Length` — the server declares how many bytes to expect before sending. But a `SELECT` against a live database returns an unknown number of rows. You don't know the size until you've executed the query and streamed all results. Can't declare it upfront.

Chunked encoding (HTTP) is the HTTP-layer solution to exactly this problem — stream data in chunks, terminate with a zero-length chunk. But on raw TCP with a custom protocol, there are no HTTP headers. The same idea applies at the frame level: stream data frames, terminate with a sentinel frame.

### How Postgres actually does it

Postgres uses a binary framing protocol (the "Frontend/Backend Protocol"). After streaming all data rows, the server sends:
- `CommandComplete` — "query executed successfully"
- `ReadyForQuery` — "connection is idle, ready for next query"

The client sees `ReadyForQuery` and knows two things simultaneously:
1. The full response has been consumed
2. The connection is safe to return to the pool

This is the wire-level equivalent of the `0\r\n` terminating chunk in HTTP chunked encoding — a sentinel that says "stream over, connection reusable."

### The minimal framing contract for a query-aware pool

Any protocol that wants connection pooling needs at minimum:
- **Query delimiter** — server knows when to stop reading and start executing
- **Response sentinel** — client knows when to stop reading and return the connection

This is why raw TCP pools (database drivers, Redis clients) always implement a framing protocol alongside the pool. The pool and the protocol are inseparable — without the sentinel, the pool can't know when a connection is safe to reuse.

---

## Shared-Host Pool — Live Demo

### Setup

3 server instances, each in a separate terminal:
```
go run server/main.go :3030
go run server/main.go :4040
go run server/main.go :5050
```

Pool knobs:
```go
MaxConnsPerHost     = 8
MaxIdleConnsPerHost = 3
MaxIdleConns        = 5
IdleConnTimeout     = 5s  // low for testing
```

Client: 10 goroutines per host = 30 total. Each goroutine calls `GetConn`, writes, sleeps 1s, reads response, calls `ReturnConn`.

**What each limit should force:**
- `MaxConnsPerHost = 8` → 2 goroutines per host block (10 - 8 = 2)
- `MaxIdleConnsPerHost = 3` → 5 connections per host closed on `ReturnConn` (8 - 3 = 5)
- `MaxIdleConns = 5` → global idle cap cuts across hosts (3 × 3 = 9 possible, but global cap is 5)

### Bug discovered: RemoteAddr vs dial addr mismatch

First run hit a deadlock:

```
goroutine 14 [chan send (nil chan)]:
pool.(*SharedConnPool).ReturnConn(...)
    shared_host_pool.go:148
```

`ReturnConn` uses `conn.RemoteAddr().String()` as the map key. When you dial `:3030`, `conn.RemoteAddr()` returns `127.0.0.1:3030` — not `:3030`. Map lookup `p.MaxConns["127.0.0.1:3030"]` returns nil. Sending to a nil channel blocks forever. Mutex never released. Every other goroutine waiting for the lock deadlocks.

**Fix:** hosts array changed to use full `ip:port` format matching what `RemoteAddr()` returns:
```go
hosts := []string{
    "127.0.0.1:3030",
    "127.0.0.1:4040",
    "127.0.0.1:5050",
}
```

**The robust fix** is `ReturnConn(addr string, conn net.Conn)` — caller passes the same addr used in `GetConn`. `RemoteAddr()` is unreliable as a map key: DNS hostnames, load balancers, IPv6 all produce different strings than the dial addr.

### Client logs

```
16:52:19.796002 [go-routine-:3030-7] blocking for MaxConnsPerHost   ← 6 blocked within 167μs
16:52:19.796086 [go-routine-:4040-7] blocking for MaxConnsPerHost
16:52:19.796087 [go-routine-:3030-4] blocking for MaxConnsPerHost
16:52:19.796122 [go-routine-:4040-4] blocking for MaxConnsPerHost
16:52:19.796148 [go-routine-:5050-7] blocking for MaxConnsPerHost
16:52:19.796169 [go-routine-:5050-8] blocking for MaxConnsPerHost
16:52:19.796100 [go-routine-:3030-1] using conn.LocalAddress(127.0.0.1:65045)   ← 24 dial within 655μs
... (24 more "using" lines, all at 16:52:19.796xxx)
16:52:20.797555 [response-from-server] for [go-routine-:5050-6]...   ← wave 1 responses, all within 413μs
... (24 response lines, all at 16:52:20.797xxx)
16:52:20.798787 [go-routine-:5050-8] using conn.LocalAddress(127.0.0.1:65071)   ← 6 unblock within 190μs
16:52:20.798811 [go-routine-:3030-7] using conn.LocalAddress(127.0.0.1:65069)
16:52:20.798910 [go-routine-:5050-7] using conn.LocalAddress(127.0.0.1:65074)
16:52:20.798919 [go-routine-:4040-4] using conn.LocalAddress(127.0.0.1:65073)
16:52:20.798926 [go-routine-:4040-7] using conn.LocalAddress(127.0.0.1:65070)
16:52:20.798977 [go-routine-:3030-4] using conn.LocalAddress(127.0.0.1:65072)
16:52:21.800958 [response-from-server] for [go-routine-:3030-4]...   ← wave 2 responses
... (6 response lines, all at 16:52:21.800xxx-801xxx)
```

Full logs in `cmd/conn-pooling/shared-host-pool-client.log`.

### Reading the logs

**`MaxConnsPerHost = 8` ✓**
- `:3030` blocked: goroutines 7, 4 — exactly 2
- `:4040` blocked: goroutines 7, 4 — exactly 2
- `:5050` blocked: goroutines 8, 7 — exactly 2
- Per-host semaphore is isolated — `:3030` blocking doesn't affect `:4040` or `:5050`

**Connection isolation ✓**
Each host got its own set of ephemeral ports — no cross-host connection sharing.

**Microsecond timing reveals the wave mechanism:**
- Launch burst: 6 blocks + 24 dials all within ~800μs — true concurrency
- Wave 1 completes: all 24 responses within 413μs of each other (1s sleep expiring simultaneously)
- Wave 2 unblocks: **~0.8ms after last wave-1 response** — this is `ReturnConn` → excess connections closed → semaphore tokens released → channel send wakes parked goroutines → `net.Dial` → "using" logged
- All 6 blocked goroutines unblock within 190μs of each other — token releases cascade fast

**`MaxIdleConns = 5` — confirmed in the final run (see below)**
After adding log messages to all `ReturnConn` rejection paths and a second round of requests, the 18:01:29 run shows all three caps firing and pool reuse proven. 3 hosts × `MaxIdleConnsPerHost = 3` = 9 possible idle connections; `MaxIdleConns = 5` is the binding constraint.

### Final Run (18:01:29) — All Caps + Pool Reuse

#### What was added before this run

- `returning conn :X to pool` — connection accepted into idle pool
- `reached max-idle for this host(:X)` — per-host idle cap hit
- `[pool-global-idle-cap]` — global idle cap hit (already existed)
- `using conn from pool and releasing global IdleConns` — pool-hit path in `GetConn`
- Round prefix `[round-N]` throughout for clarity
- `ReturnConn(round int, addr string, conn net.Conn)` — addr passed by caller to avoid `RemoteAddr()` mismatch

#### Round-1 — all three caps firing

```
18:01:29.044620 [round-1][go-routine-:3030-7] blocking for MaxConnsPerHost   ← 6 blocked (2 per host)
18:01:29.044950 [round-1][go-routine-:3030-9] blocking for MaxConnsPerHost
18:01:29.045045 [round-1][go-routine-:4040-7] blocking for MaxConnsPerHost
18:01:29.044971 [round-1][go-routine-:4040-1] blocking for MaxConnsPerHost
18:01:29.045337 [round-1][go-routine-:5050-1] blocking for MaxConnsPerHost
18:01:29.045340 [round-1][go-routine-:5050-6] blocking for MaxConnsPerHost
18:01:29.045047 [round-1][go-routine-:5050-9] using conn.LocalAddress(127.0.0.1:49915)   ← 24 dial (8 per host)
... (23 more "using" lines)

18:01:30.046039 [round-1][response-from-server] for [go-routine-:5050-9]...   ← wave-1: 24 complete
... (23 more response lines)
18:01:30.046078 [round-1] returning conn :5050 to pool    ← pool accepts 5 connections total
18:01:30.046153 [round-1] returning conn :4040 to pool
18:01:30.046254 [round-1] returning conn :4040 to pool
18:01:30.046276 [round-1] returning conn :3030 to pool
18:01:30.046337 [round-1] returning conn :3030 to pool
18:01:30.047846 [round-1] [pool-global-idle-cap] releasing the conn to MaxConnsPerHost and closing conn: 127.0.0.1:3030
... (19 more [pool-global-idle-cap] entries across all 3 hosts)

18:01:30.048246 [round-1][go-routine-:3030-7] using conn.LocalAddress(127.0.0.1:49939)   ← 6 blocked goroutines unblock
... (5 more, all within ~2ms)
18:01:31.049446 [round-1][response-from-server] for [go-routine-:3030-7]...   ← wave-2 responses
18:01:31.049496 [round-1] [pool-global-idle-cap] ...   ← wave-2 connections also hit global cap
```

**Counting each cap:**
- `blocking for MaxConnsPerHost`: 6 = 2 per host × 3 hosts ✓ (`MaxConnsPerHost = 8`, 10 goroutines → 2 block per host)
- `returning conn X to pool`: **5** = exactly `MaxIdleConns` ✓
- `[pool-global-idle-cap]`: all remaining connections rejected by global cap
- `reached max-idle for this host`: absent in this run — see non-determinism note below

#### Non-deterministic global idle slot distribution

3 × `MaxIdleConnsPerHost` (3) = 9 connections *could* sit idle. `MaxIdleConns` (5) is the global ceiling. Which host gets how many idle slots is a race: whichever goroutines call `ReturnConn` first after their 1s sleep wins.

Two consecutive runs with identical knobs show different distributions:

| Run | `:3030` slots | `:4040` slots | `:5050` slots | `reached max-idle` |
|-----|:---:|:---:|:---:|---|
| 18:01:29 | 2 | 2 | 1 | absent |
| 18:20:59 | 3 | 1 | 1 | `:3030` in both rounds |

**18:01:29** — global cap (5) filled before any host accumulated 3 slots. All rejections went through `[pool-global-idle-cap]`; `reached max-idle` never fired.

**18:20:59** — `:3030`'s goroutines happened to call `ReturnConn` first and accumulated all 3 of their per-host slots. The fourth `:3030` connection then hit `reached max-idle for this host(:3030)`. The global cap filled shortly after, pushing remaining `:4040` and `:5050` connections through `[pool-global-idle-cap]`. In round-2 of this run, `:3030` again held 3 idle slots and re-hit the per-host cap when returning:
```
18:21:02.184591 [round-2] reached max-idle for this host(:3030), releasing conn to MaxConnsPerHost and closing conn: 127.0.0.1:3030
18:21:02.184934 [round-2] reached max-idle for this host(:3030), releasing conn to MaxConnsPerHost and closing conn: 127.0.0.1:3030
```

Same knobs, different goroutine scheduling, different log paths. **The invariant holds either way: pool never exceeds `MaxIdleConns = 5` total.**

#### Round-2 — pool hit proof

Round-2 launches 5 goroutines per host. From the 18:20:59 run (`:3030` held 3 slots, `:4040` and `:5050` held 1 each):

```
18:21:01.183548 [round-2] [go-routine-:3030-0] using conn (127.0.0.1:50261) from pool and releasing global IdleConns
18:21:01.183569 [round-2][go-routine-:3030-0] using conn.LocalAddress(127.0.0.1:50261)   ← same port, redundant now
18:21:01.183574 [round-2] [go-routine-:4040-3] using conn (127.0.0.1:50255) from pool and releasing global IdleConns
18:21:01.183614 [round-2] [go-routine-:5050-4] using conn (127.0.0.1:50264) from pool and releasing global IdleConns
18:21:01.183682 [round-2] [go-routine-:3030-1] using conn (127.0.0.1:50254) from pool and releasing global IdleConns
18:21:01.183756 [round-2] [go-routine-:3030-3] using conn (127.0.0.1:50260) from pool and releasing global IdleConns
18:21:01.184330 [round-2][go-routine-:3030-4] using conn.LocalAddress(127.0.0.1:50282)   ← new dial (no pool log above it)
... (9 more new dials)
```

**The proof:** port `50261` was assigned by the kernel to `:3030-0`'s round-1 connection. In round-2 it reappears on `:3030-0` — the same goroutine happened to get its own connection back from the pool. Same local port = same `tcp_sock` = zero new TCP handshakes.

The pool-hit log now carries the port inline — the `using conn (127.0.0.1:50261) from pool` line is fully self-documenting. The `using conn.LocalAddress` line from `main.go` is now redundant for pool hits; it's still useful for the fresh-dial path where the pool layer logs nothing.

All 5 pool hits complete within **208μs** (`183548 → 183756`). Pool access cost is negligible compared to dial cost (TCP handshake ≥ 1 RTT).

Remaining 10 goroutines in round-2 dialed fresh connections. Round-2 also triggered both `reached max-idle for this host(:3030)` and `[pool-global-idle-cap]` on return — identical cap behaviour to round-1 since the pool state was rebuilt to the same configuration.

---

### Wave pattern — cross-host version

The wave pattern from the dedicated pool repeats here, now running simultaneously across 3 independent host semaphores:

```
T=0s:    8 goroutines per host dial    (24 total in-flight)
         2 goroutines per host block   (6 total waiting)
T=1s:    24 complete, ReturnConn called
         pool accepts 3 per host (MaxIdleConnsPerHost)
         5 per host closed → 5 semaphore tokens released per host
         6 blocked goroutines unblock (2 per host)
T=2s:    6 complete, ReturnConn called
         pool may accept more (slots freed by wave-2 goroutines)
```

Each host's semaphore is independent — one host's load doesn't affect another's blocking behavior.

---

## HTTP vs Raw TCP — Framing and Pooling Compared

Built in `cmd/http-pooling/` — an HTTP echo server and client using `net/http` and `httptrace`, to compare what `net/http` does transparently against what was built manually in the raw TCP pool.

### The setup

**Server:** `net/http` handler that reads `r.Body` and echoes it back as JSON. `h2c.NewHandler` wraps the mux as the outermost layer so the server negotiates HTTP/2 cleartext (h2c) when the client supports it, while staying fully compatible with HTTP/1.1 clients:

```go
mux := http.NewServeMux()
mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
    defer r.Body.Close()
    s := strings.Builder{}
    sc := bufio.NewScanner(r.Body)
    for sc.Scan() {
        s.Write(sc.Bytes())
    }
    if s.String() == "slow" {
        time.Sleep(5 * time.Second)
    }
    json.NewEncoder(w).Encode(struct {
        Echo string `json:"echo"`
    }{Echo: s.String()})
})

return http.Server{
    Addr:    ":4040",
    Handler: h2c.NewHandler(mux, &http2.Server{}),  // h2c wraps mux, not the other way
}
```

`h2c.NewHandler` must be at the **outermost** `Handler` position — it reads the first bytes of the connection to detect the HTTP/2 preface (`PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n`). If detected, it hands off to `http2.Server`; otherwise it falls through to `mux` as a plain HTTP/1.1 connection. Placing it inside a route handler would mean the preface detection runs after `net/http` has already parsed the request as HTTP/1.1.

**Client:** `http.Client` with explicit `http.Transport` knobs (same values as the raw pool: `MaxIdleConnsPerHost=3`, `MaxIdleConns=5`, `MaxConnsPerHost=8`). Uses `httptrace.ClientTrace` to observe connection lifecycle:

```go
trace := &httptrace.ClientTrace{
    ConnectStart: func(network, addr string) {
        log.Printf("ConnectStart: %s:%s", network, addr)
    },
    GotConn: func(info httptrace.GotConnInfo) {
        log.Printf("GotConn: conn.LocalAddress(%v) reused=%v idleTime=%v",
            info.Conn.LocalAddr(), info.Reused, info.IdleTime)
    },
}
ctx := httptrace.WithClientTrace(context.Background(), trace)
req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost:4040/echo", body)
res, _ := client.Do(req)
```

`ConnectStart` fires only for new TCP dials — never for pool hits. `GotConn` fires for every request, with `Reused bool` and `IdleTime` for pool-hit connections.

### Concurrent run — `MaxConnsPerHost` in action

10 goroutines fire simultaneously, each posting to `/echo`:

```go
client := http.Client{
    Transport: &http.Transport{
        MaxIdleConnsPerHost: 3,
        MaxIdleConns:        5,
        MaxConnsPerHost:     8,
        IdleConnTimeout:     10 * time.Second,
    },
}

trace := httptrace.ClientTrace{
    ConnectStart: func(network, addr string) {
        log.Printf("ConnectStart: %s:%s", network, addr)
    },
    GotConn: func(gci httptrace.GotConnInfo) {
        log.Printf("GotConn: conn.LocalAddress(%s) for conn.RemoteAddress(%s)",
            gci.Conn.LocalAddr(), gci.Conn.RemoteAddr())
    },
}
ctx := httptrace.WithClientTrace(context.Background(), &trace)

sg := sync.WaitGroup{}
for i := range 10 {
    sg.Add(1)
    go func(i int) {
        defer sg.Done()

        speed := strings.NewReader("fast")
        if i == 4 {
            speed = strings.NewReader("slow")
        }

        r, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:4040/echo", speed)
        if err != nil {
            return
        }
        res, err := client.Do(r)
        if err != nil {
            return
        }
        defer res.Body.Close()

        sc := bufio.NewScanner(res.Body)
        s := strings.Builder{}
        for sc.Scan() {
            s.Write(sc.Bytes())
        }
        log.Printf("[go-routine-%d] response-from-server: %s", i, s.String())
    }(i)
}
sg.Wait()
```

```
21:43:20 ConnectStart: tcp:[::1]:4040    ←┐
21:43:20 ConnectStart: tcp:[::1]:4040    ← ┤
21:43:20 ConnectStart: tcp:[::1]:4040    ← ┤  8 dials, simultaneous
21:43:20 ConnectStart: tcp:[::1]:4040    ← ┤  (MaxConnsPerHost = 8)
21:43:20 ConnectStart: tcp:[::1]:4040    ← ┤
21:43:20 ConnectStart: tcp:[::1]:4040    ← ┤
21:43:20 ConnectStart: tcp:[::1]:4040    ← ┤
21:43:20 ConnectStart: tcp:[::1]:4040    ←┘

21:43:20 GotConn: [::1]:52621           ←┐
21:43:20 GotConn: [::1]:52622           ← ┤  8 connections established
21:43:20 GotConn: [::1]:52624           ← ┤  (8 distinct ports from kernel)
21:43:20 GotConn: [::1]:52623           ← ┤
21:43:20 GotConn: [::1]:52627           ← ┤
21:43:20 GotConn: [::1]:52628           ← ┤
21:43:20 GotConn: [::1]:52626           ← ┤
21:43:20 GotConn: [::1]:52625           ←┘

21:43:20 response: {"echo":"hi"}        ← 8 responses arrive
21:43:20 response: {"echo":"hi"}
21:43:20 GotConn: [::1]:52627           ← request 9: reused port 52627 (no ConnectStart)
21:43:20 response: {"echo":"hi"}
21:43:20 GotConn: [::1]:52628           ← request 10: reused port 52628 (no ConnectStart)
21:43:20 response: {"echo":"hi"}
...
```

**What happened:** 10 goroutines hit the pool at the same moment. `MaxConnsPerHost=8` caps simultaneous connections — so 8 dial immediately, 2 are held by the semaphore. As soon as the first responses arrive and connections are returned to the pool, the 2 waiting goroutines grab them (port 52627, 52628 reused — no new `ConnectStart`).

Same wave pattern as the raw `SharedConnPool`: burst → cap → wait → reuse. The difference: no manual semaphore code. `http.Transport` implemented the cap transparently.

### Sequential run — pool reuse confirmed

```
ConnectStart: tcp:[::1]:4040              ← one dial, ever
GotConn: conn.LocalAddress([::1]:52648)   ← request 1: new connection
GotConn: conn.LocalAddress([::1]:52648)   ← request 2: reused (no ConnectStart above it)
GotConn: conn.LocalAddress([::1]:52648)   ← request 3: reused
...                                        ← requests 4–10: all port 52648
```

One TCP handshake. Ten requests. Nine reuses. No pool code written — `http.Transport` handled it entirely.

The proof is identical to the raw pool: same ephemeral port across all requests = same `tcp_sock` reused. The difference is `net/http` returned the connection to the pool transparently, driven by the response framing headers.

### `Connection: close` — pool disabled from the server

Added `w.Header().Set("Connection", "close")` to the handler:

```
ConnectStart → GotConn [::1]:52712 → response   ← request 1
ConnectStart → GotConn [::1]:52713 → response   ← request 2
ConnectStart → GotConn [::1]:52714 → response   ← request 3
...                                               ← 10 requests, 10 dials
```

Ports incrementing sequentially — kernel allocating a fresh `tcp_sock` per request. `http.Transport` reads the response, sees `Connection: close`, discards the connection instead of returning it to the pool. Next request: pool empty → new `ConnectStart`.

This is HTTP/1.0 behaviour — what every HTTP request looked like before keep-alive existed.

### The realisation: `Connection: keep-alive` is redundant in HTTP/1.1

The server without any `Connection` header pooled connections correctly. Removing `w.Header().Set("Connection", "keep-alive")` changed nothing observable.

HTTP/1.1 **flipped the default from HTTP/1.0**:
- HTTP/1.0: default is `close`. Keep-alive requires explicit opt-in via `Connection: keep-alive`.
- HTTP/1.1: default is keep-alive. Close requires explicit opt-out via `Connection: close`.

The minimal decision tree for `http.Transport`:

```
server sends Connection: close      →  discard connection (do not pool)
server sends nothing                →  return to pool (HTTP/1.1 default)
server sends Connection: keep-alive →  return to pool (redundant, same as nothing)
```

### Framing comparison — raw TCP vs HTTP

| | Request end | Response end | Connection fate |
|---|---|---|---|
| Raw TCP | `\n` (newline) | implicit — echo is always one line | server loop stays open |
| HTTP | `\r\n\r\n` after headers + Content-Length or `0\r\n\r\n` | Content-Length bytes read or chunked terminator | `Connection` header decides |

**Raw TCP framing is implicit** — both sides must know the protocol in advance. Works because you control both client and server.

**HTTP framing is self-describing** — the message carries its own length and connection intent via headers. Works across the entire internet because any conforming client and server can interoperate without knowing each other's implementation.

### What `net/http` does transparently

Everything built manually in the raw pool maps directly to something inside `net/http`:

| Raw TCP (built manually) | HTTP (`net/http`) |
|---|---|
| `for sc.Scan()` server loop | inside `net/http` server — transparent to handler |
| `\n` request delimiter | `\r\n\r\n` after HTTP headers |
| implicit one-line response sentinel | `Content-Length` or `Transfer-Encoding: chunked` |
| `SharedConnPool` with semaphores | `http.Transport` with same knobs |
| `ReturnConn` checking Connection: close | Transport reads response headers, decides pool vs discard |

Building the raw pool first made the `net/http` black box transparent — the knobs, the framing contract, the server loop decision are no longer magic.

---

## HTTP/1.1 Head-of-Line (HOL) Blocking

### The structural constraint

HTTP/1.1 with keep-alive reuses one TCP connection for multiple requests — but strictly one request in-flight at a time. The connection is a single ordered byte stream. While a response is in-flight, the connection is occupied and unavailable for any other request.

### Live demo

Server handler sleeps 5 seconds when body is `"slow"`. Client sends sequentially: `fast, fast, fast, fast, slow, fast, fast, fast, fast, fast`.

```
22:54:46  ConnectStart → :53594         ← one dial, one connection for all requests
22:54:46  GotConn :53594 → fast         ← instant
22:54:46  GotConn :53594 → fast         ← instant
22:54:46  GotConn :53594 → fast         ← instant
22:54:46  GotConn :53594 → fast         ← instant
22:54:46  GotConn :53594 → [waiting]    ← slow request holds the connection
22:54:51  response: slow                ← 5 seconds later, connection released
22:54:51  GotConn :53594 → fast         ← all unblock instantly
22:54:51  GotConn :53594 → fast
22:54:51  GotConn :53594 → fast
22:54:51  GotConn :53594 → fast
22:54:51  GotConn :53594 → fast
```

One connection, port `53594` reused for all 10 requests. The slow request held the connection hostage for 5 seconds. Five trivially fast requests sat idle — not because the server was busy, but because the **shared connection was occupied**.

**Debugging note:** an earlier run of this experiment showed a `ConnectStart` per request instead of one. Cause: `Connection: close` was still set on the server from a previous experiment — Transport discarded the connection after every response, making the "sequential wait" just client code rather than true HOL blocking on a shared connection. Removing it revealed the real behaviour.

### How browsers work around it — multiple parallel connections

Browsers open **6 parallel TCP connections** per host (per spec). A slow request only blocks its own connection, not the other 5. With 30 resources:

```
connection 1: resource 1, 7,  13, 19, 25   ← HOL within this connection
connection 2: resource 2, 8,  14, 20, 26
connection 3: resource 3, 9,  15, 21, 27
connection 4: resource 4, 10, 16, 22, 28
connection 5: resource 5, 11, 17, 23, 29
connection 6: resource 6, 12, 18, 24, 30
```

Each connection has its own queue. A slow resource blocks only its queue. But the cost: 6 × TCP handshake overhead, and HOL blocking still exists within each connection.

**Domain sharding** — some sites serve static assets from multiple subdomains (`cdn1.example.com`, `cdn2.example.com`) to get more than 6 connections. A hack around the per-host connection limit.

### Why HTTP pipelining didn't fix it

HTTP pipelining: client sends all requests without waiting for responses. Looks like it solves HOL blocking — no idle time between requests.

```
client → server:  req1, req2, req3(slow), req4, req5   (all sent immediately)
server → client:  resp1, resp2, resp3(slow)...          (must come in order)
```

The server must respond **in request order**. TCP delivers bytes in sequence, and HTTP/1.1 has no way to label which response belongs to which request. Even if resp4 and resp5 are ready on the server, they can't be sent — resp3 hasn't gone out yet. Pipelining moved HOL blocking from the request side to the response side. Same connection, same bottleneck.

In practice, pipelining was disabled by most browsers and proxies — proxies that didn't understand pipelining would incorrectly match responses to requests, causing data corruption.

### The root cause — ordering without identity

The fundamental problem: HTTP/1.1 has no way to label frames. The connection is a single ordered stream. The client and server must process messages in the same order or the byte stream becomes ambiguous.

Stated as a principle: **ordering without identity = HOL blocking**. Whenever multiple logical flows share a single ordered channel with no per-flow label, one slow flow blocks all others. The fix is always the same: add identity so flows can be reordered, interleaved, or routed independently.

This pattern appears everywhere in systems:

| System | Shared channel | Identity token | What identity unlocks |
|---|---|---|---|
| HTTP/1.1 | TCP byte stream | ❌ none | 6 parallel connections as a workaround |
| HTTP/2 | TCP byte stream | ✅ 31-bit stream ID per frame | Multiplexing N streams on 1 connection |
| HTTP/3/QUIC | UDP datagrams | ✅ stream ID + connection ID | Per-stream reliability; lost packet only stalls its own stream |
| Database locks | Row/page latch | Row-level lock key | Fine-grained locking; one writer doesn't block others |
| Event loops | Callback queue | Microtask vs macrotask priority | Non-blocking I/O; slow callback doesn't stall the loop |
| CPU pipelines | Instruction stream | Register renaming / ROB slot | Out-of-order execution; independent instructions proceed |

Every workaround — multiple connections, pipelining, domain sharding — is working around this absence of frame identity, not fixing it. This is exactly what HTTP/2 addresses.

---

## HTTP/2 — Multiplexing

### What HTTP/2 changed at the frame level

HTTP/2 is a binary framing layer over TCP. Every message is cut into **frames**, and every frame carries a **31-bit stream ID**.

```
HTTP/2 frame layout (9-byte fixed header + payload):
┌──────────────────────────────────────┐
│  Length   (24 bits)                  │  payload byte count
│  Type     (8 bits)                   │  DATA, HEADERS, SETTINGS, WINDOW_UPDATE ...
│  Flags    (8 bits)                   │  END_STREAM, END_HEADERS, PADDED ...
│  R + Stream ID (1 + 31 bits)         │  which logical stream this frame belongs to
├──────────────────────────────────────┤
│  Payload  (0–16 383 bytes)           │
└──────────────────────────────────────┘
```

Stream ID 0 is reserved for connection-level control (SETTINGS, PING, GOAWAY). All request/response streams use odd IDs (1, 3, 5 …) assigned by the client. The server assigns even IDs for server-push streams.

### Multiplexing: N streams, 1 connection

Because every frame carries its own stream ID, the receiver reconstructs each logical request/response independently. Frames from different streams can be freely interleaved on the wire.

```
wire bytes (simplified):
[stream=1 HEADERS] [stream=3 HEADERS] [stream=1 DATA] [stream=5 HEADERS]
[stream=3 DATA]    [stream=5 DATA(slow...)]  [stream=1 END]  [stream=3 END]
... stream 5 still going ...  [stream=5 END]
```

Stream 5 (slow) is in-flight while streams 1 and 3 complete independently. The server writes frames for ready streams without waiting for slow ones. HTTP/1.1 pipelining could send requests without waiting, but responses had to come back in order. HTTP/2 breaks that constraint by labeling every frame.

### Attempt log — server upgraded to h2c, client still HTTP/1.1

Server wired with `h2c.NewHandler`. Client still uses `http.Transport` — no h2c negotiation, so the server falls through to HTTP/1.1 for every connection.

```
23:46:32 ConnectStart × 8               ← still 8 dials (http.Transport, not http2.Transport)
23:46:32 GotConn: [::1]:54470
23:46:32 GotConn: [::1]:54469
23:46:32 GotConn: [::1]:54471
23:46:32 GotConn: [::1]:54476
23:46:32 GotConn: [::1]:54475
23:46:32 GotConn: [::1]:54473
23:46:32 GotConn: [::1]:54474
23:46:32 GotConn: [::1]:54472

23:46:32 [go-routine-7] response: fast   ←┐
23:46:32 GotConn: [::1]:54471 (reused)   ← ┤  fast goroutines complete
23:46:32 [go-routine-2] response: fast   ← ┤  immediately; slow goroutine
23:46:32 GotConn: [::1]:54476 (reused)   ← ┤  has its OWN connection
23:46:32 [go-routine-1] response: fast   ← ┤  so it doesn't block anyone
23:46:32 [go-routine-5] response: fast   ← ┤
23:46:32 [go-routine-0] response: fast   ← ┤
23:46:32 [go-routine-9] response: fast   ← ┤
23:46:32 [go-routine-3] response: fast   ← ┤
23:46:32 [go-routine-8] response: fast   ←┘

23:46:37 [go-routine-4] response: slow   ← 5 seconds later, on its own connection
```

**go-routine-4 doesn't block anyone** — because with concurrent requests and `MaxConnsPerHost=8`, all 8 goroutines dialed their own connections. Go-routine-4's `"slow"` request holds port `54475` (its own connection) while all others proceed on separate connections. This is the same "6 parallel connections" workaround browsers use — HOL blocking is isolated to the slow goroutine's connection only.

This reveals the difference between the two experiments:
- **HOL blocking demo (earlier):** sequential requests on a single shared connection — slow blocks everyone waiting for that one connection
- **Concurrent + h2c attempt (this run):** each goroutine got its own connection — slow only blocks itself

True HTTP/2 multiplexing would show **1 `ConnectStart`**, all goroutines on **1 port**, and the slow goroutine still not blocking fast ones — because stream IDs let the server interleave responses on the same TCP connection.

**Next step:** upgrade client to `http2.Transport` with `AllowHTTP: true` and a plain `net.Dial` in `DialTLSContext`.

### The irony — TCP HOL blocking remains

HTTP/2 eliminated application-layer HOL blocking. But it still sits on TCP, which is an ordered byte stream. When a TCP segment is lost, the kernel holds all subsequent in-order segments in the receive buffer until retransmission. This stalls the application read — **all HTTP/2 streams block**, even those whose data arrived intact.

HTTP/1.1 opens 6 connections per host. A packet loss event on one connection stalls only that connection; the other 5 keep delivering. HTTP/2 with 1 connection: a single packet loss stalls all streams simultaneously. Under high packet-loss conditions (mobile networks, congested links), HTTP/2 can actually be **slower** than HTTP/1.1.

| | HTTP/1.1 | HTTP/2 | HTTP/3 |
|---|---|---|---|
| Transport | TCP (6 connections) | TCP (1 connection) | QUIC over UDP |
| App-layer HOL blocking | ✅ yes, per connection | ❌ eliminated | ❌ eliminated |
| Transport-layer HOL blocking | contained — 1 of 6 connections stalls | worse — all streams stall | ❌ eliminated — per-stream |
| Handshake cost | TCP × 6 | TCP × 1 | 0-RTT possible |
| Under packet loss | only 1 of 6 connections pauses | every stream pauses | only the affected stream pauses |

### HTTP/3 and the QUIC solution

QUIC runs over UDP and implements per-stream reliability itself. If a QUIC packet carrying stream-5 data is lost, only stream 5 pauses. Streams 1, 3, and 7 receive their data and continue — the OS never sees a hole in a shared byte stream because QUIC owns the ordering guarantee per stream.

Same "identity solves ordering" principle applied at the transport layer: QUIC gives each stream independent sequence numbers, so a gap in one stream's sequence space is invisible to others.

---

## Where This Goes Next

- Observability: instrument pool hits, misses, new dials, evictions, pool size over time → Grafana
- Load test: pool vs no-pool latency comparison
- Protocol design: implement minimal framing on top of the echo server — query delimiter + response sentinel
- Robust `ReturnConn`: the current fix (passing `addr` explicitly) works but documents the `RemoteAddr()` footgun; a production pool would enforce this via the type system

---

*This document will be updated as the session progresses.*
