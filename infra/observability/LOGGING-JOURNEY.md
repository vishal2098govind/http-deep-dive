# Logging Journey

A deep dive into what logging actually is under the hood — from "it's just fmt.Println" to concurrent writes, mutex bottlenecks, buffer pooling, GC interaction, and the systems programming pattern that ties it all together.

This document is the source material for a Substack article.

---

## The Misconception

Most developers think logging is trivial:

```go
log.Info("request ended")
```

One line. How hard can it be?

Turns out logging under high concurrency is a microcosm — a small thing that contains and reflects all the complexity of a much larger thing — of systems programming problems:
- Concurrent writes to a shared file descriptor
- Mutex contention under traffic spikes
- Memory allocation pressure from scratch buffers
- GC interaction with pooled objects
- The same pattern used in HTTP clients, database drivers, and serialization libraries

---

## Chapter 1: What a Log Write Actually Is

When you call:

```go
log.Info(ctx, "Request ended", "duration", "8.2s", "status", 200)
```

Three things happen:

**1. Formatting** — individual fields are assembled into a byte sequence:
```
"duration" + "8.2s" + "status" + 200
        ↓
{"time":"...","level":"INFO","msg":"Request ended","duration":"8.2s","status":200}
```
This assembled byte sequence lives in a scratch buffer in memory while being built.

**2. Write** — the formatted bytes are written to fd=1 (stdout):
```go
w.Write(buf)  // syscall → kernel → fd=1 → wherever stdout points
```

**3. Discard** — the scratch buffer is done, goes out of scope.

Simple enough for one goroutine. Now add 1000 concurrent request handlers all logging simultaneously.

---

## Chapter 2: The Concurrent Write Problem

Two goroutines writing to fd=1 simultaneously without coordination:

```
goroutine A: {"level":"INFO","msg":"Request en
goroutine B:                                   {"level":"ERROR","msg":"upload failed"}
goroutine A:                                                                          ded","duration":"8.2s"}
```

What lands on disk:
```
{"level":"INFO","msg":"Request en{"level":"ERROR","msg":"upload failed"}ded","duration":"8.2s"}
```

Not valid JSON. Log parsers break. Loki can't index fields. Grafana shows garbage. A real production incident becomes unreadable at the worst possible moment.

**The fix: a mutex around the write.**

`slog`'s `JSONHandler` does exactly this:

```go
type JSONHandler struct {
    mu *sync.Mutex
    w  io.Writer
}

func (h *JSONHandler) Handle(ctx context.Context, r Record) error {
    h.mu.Lock()
    defer h.mu.Unlock()
    // format entire record into internal buffer
    // write complete JSON line to w
}
```

One goroutine holds the mutex, formats and writes, releases. Others wait. Each log line lands as a complete JSON object — no interleaving.

---

## Chapter 3: The Mutex Bottleneck

The mutex solves correctness. But it covers the entire format+write operation:

```
[goroutine A holds mutex: building JSON string... writing... done]
[goroutine B waits                                               ]
[goroutine C waits                                               ]
```

Under 1000 concurrent goroutines, 999 are waiting while 1 formats. The expensive part — assembling the JSON string — is serialized even though it could be done in parallel. Each goroutine is working with its own data, there's no reason they can't format simultaneously.

**The insight:** formatting doesn't need shared state. Only the write to fd=1 needs coordination.

```
slog:    [mutex: format + write]          ← mutex too wide
zerolog: format (no mutex) → [mutex: write only]  ← mutex as narrow as possible
```

---

## Chapter 4: zerolog's Approach — Format Outside the Lock

zerolog moves formatting outside the mutex by giving each goroutine its own scratch buffer to format into:

```go
// goroutine A                    // goroutine B
buf_A := pool.Get()               buf_B := pool.Get()
buf_A.Write(`{"level":"INFO"...`) buf_B.Write(`{"level":"ERROR"...`)
// both formatting in parallel, no contention

mutex.Lock()                      // B waits here
w.Write(buf_A)
mutex.Unlock()
                                  mutex.Lock()
                                  w.Write(buf_B)
                                  mutex.Unlock()
```

Formatting happens in parallel. The mutex window shrinks to just the write syscall — microseconds instead of milliseconds.

**But where do the scratch buffers come from?**

The naive approach: allocate a fresh `[]byte` for every log call. Under 1000 requests/sec, that's 1000 allocations/sec just for log buffers — continuous GC pressure. This is where `sync.Pool` comes in.

---

## Chapter 5: `sync.Pool` — Object Pooling for Scratch Buffers

`sync.Pool` is a pool of reusable objects. Instead of allocating and discarding:

```
allocate → use → discard → GC collects → allocate → use → discard → GC collects ...
```

You reuse:
```
allocate once → use → reset → return to pool → use → reset → return to pool ...
```

Basic usage:
```go
var pool = &sync.Pool{
    New: func() any {
        return bytes.NewBuffer(make([]byte, 0, 1024))
    },
}

buf := pool.Get().(*bytes.Buffer)   // checkout — reuse existing or allocate new
buf.WriteString(`{"level":"INFO"}`) // format into it
// ...write to fd=1...
buf.Reset()                          // clear contents, keep underlying memory
pool.Put(buf)                        // checkin — available for next caller
```

The key: `buf.Reset()` clears the contents but keeps the allocated `[]byte` underneath. The memory is reused, not garbage collected and reallocated.

---

## Chapter 6: Why `sync.Pool` Is Lock-Free in the Common Case

A naive pool would use a mutex-protected slice. Under high concurrency, goroutines would contend on that mutex — no better than what we started with.

`sync.Pool` is smarter. It exploits Go's scheduler structure.

**Go's scheduler has P processors** (one per CPU core by default). Every goroutine runs on a P. `sync.Pool` gives each P its own local slot:

```
Pool
  local[0] → buf   ← P0's slot
  local[1] → buf   ← P1's slot
  local[2] → nil   ← P2's slot, empty
  local[3] → buf   ← P3's slot
```

When a goroutine calls `pool.Get()`:
1. Which P am I on? Say P0.
2. Is `local[0]` non-nil? Yes → take it. Done. **No lock needed.**
3. If nil → check other P's slots (needs lock) → if all empty → call `New`

When a goroutine calls `pool.Put(buf)`:
1. Which P am I on? Say P1.
2. Put buf into `local[1]`. Done. **No lock needed.**

Under normal traffic, each P's goroutines use their own slot — no cross-P contention. `Get` and `Put` are array reads and writes.

**Pool can hold more than N buffers — poolChain:**

One slot per P is the fast path. Each P also has a `poolChain` — a lock-free overflow queue for when the local slot is already occupied. Under a traffic spike with 1000 concurrent goroutines, 1000 buffers circulate across local slots and poolChains. Pool size is unbounded — limited only by RAM, self-tuning to actual concurrency.

---

## Chapter 7: The GC Problem and Victim Cache

The pool grows under spikes. After the spike subsides, those buffers sit idle. If the GC never collected them, memory would grow to the spike's high watermark and stay there permanently — a memory leak.

But if the GC wipes all buffers every cycle:
```
GC runs → all pool buffers collected
Next Get() → pool empty → 1000 New() calls → allocation spike
More allocations → more GC → more spikes → bad cycle
```

`sync.Pool` solves this with a **two-generation victim cache**.

The pool has two arrays:
```
local[]   — current cycle's buffers (fast path)
victim[]  — previous cycle's buffers (one extra cycle of grace)
```

Before each GC cycle, the runtime calls `poolCleanup()`:
```go
func poolCleanup() {
    victim = nil      // drop reference → old victim collected by GC
    victim = local    // promote local to victim — give buffers one more cycle
    local = nil       // local becomes empty
}
```

After GC, `Get()` sequence:
```
local[P]  → miss (just cleared)
victim[P] → hit  → buffer reused, no allocation ← one cycle of grace
```

Buffers survive **two GC cycles** after going idle. Memory reclaim takes two cycles, but no allocation spike. After a traffic spike:

```
Spike ends:    1000 buffers idle in local + poolChain
GC cycle 1:    local promoted to victim (1000 buffers still alive in victim)
GC cycle 2:    victim freed (1000 buffers collected by GC)
Steady state:  pool settles back to ~concurrency-level buffers
```

The trade-off: slightly delayed memory reclaim in exchange for smooth post-GC behaviour. A deliberate design choice.

**The struct from Go's source:**
```go
type Pool struct {
    local     unsafe.Pointer // [P]poolLocal — per-P slots
    localSize uintptr
    victim     unsafe.Pointer // previous cycle's local
    victimSize uintptr
    New func() any
}
```

`unsafe.Pointer` instead of a typed slice because P count is only known at runtime (`GOMAXPROCS`), not compile time.

---

## Chapter 8: The Full zerolog Picture

```
Get buffer from pool     → lock-free, own P's slot
Format JSON into buffer  → no lock, goroutines run in parallel
mutex.Lock()             → narrow window, just the syscall
Write buffer to fd=1     → one complete JSON line, no interleaving
mutex.Unlock()
buf.Reset()
pool.Put(buf)            → lock-free, returned to own P's slot
```

Formatting — the expensive part — happens entirely outside the mutex, in parallel across all goroutines. The mutex protects only the write syscall window. Under high concurrency this is dramatically faster than `slog`'s approach.

---

## Chapter 9: Buffer Pooling Is Everywhere

This pattern — **object pooling for scratch buffers** — is not unique to logging. It's a foundational systems programming technique. The same problem (frequent short-lived allocations under concurrency) appears everywhere:

### HTTP request/response body buffers

When Go's `net/http` receives a request, it reads raw TCP bytes into a buffer before parsing headers, body, etc. Thousands of concurrent connections = thousands of concurrent buffer allocations without pooling. Go's HTTP server uses `sync.Pool` internally for exactly this.

```
TCP socket → pool buffer → HTTP parser → request struct
                ↑ pooled
```

### JSON encode/decode scratch buffers

`json.Marshal` builds a `[]byte` while encoding your struct to JSON. `json.Unmarshal` needs scratch space for tokenizing the input. Both operations are short-lived and happen on every API request.

```
Go struct → scratch buffer (JSON bytes being built) → write to TCP socket
                ↑ pooled
```

The buffer contains the in-progress JSON string — exists only for the duration of the marshal call, then done. Libraries like `jsoniter` and `easyjson` use pools aggressively for this reason.

### Database query result buffers

The database driver reads raw bytes from the TCP connection (the database protocol wire format) into a scratch buffer, then deserializes into your Go structs. The buffer is the temporary home for raw wire data between "arrived from network" and "parsed into application types."

```
TCP bytes (DB wire protocol) → pool buffer → deserializer → your Go struct
                                    ↑ pooled
```

Under concurrent queries, each goroutine needs its own buffer — pool provides them without per-query allocation cost.

### Protobuf marshalling buffers

Same as JSON — protobuf encoding builds a byte sequence before writing to the network. `protoc`-generated Go code uses pools internally for the scratch buffer.

### The general pattern

Any time you see:
- A scratch buffer allocated at the start of an operation
- Used temporarily to hold intermediate bytes
- Discarded when the operation completes
- Under concurrent load

→ That's a candidate for `sync.Pool`.

The smell in code: `make([]byte, ...)` or `new(SomeStruct)` inside a hot path that runs thousands of times per second.

---

## Chapter 10: Object Pooling vs Connection Pooling

**Connection pooling** — reuse TCP connections instead of creating new ones per request. Creating a TCP connection is expensive: 3-way handshake, TLS negotiation, kernel buffer allocation.

**Buffer/object pooling** — reuse memory allocations instead of allocating per operation. Creating a buffer is cheap individually but expensive at scale: GC pressure, allocation spikes, increased latency variance.

Same underlying idea: **expensive-to-create resource, reuse instead of recreate.** Different resources, same pattern.

| | Connection Pool | Buffer Pool |
|---|---|---|
| Resource | TCP connection | Memory allocation |
| Expense avoided | Handshake + kernel setup | GC pressure + allocation |
| Go primitive | `http.Transport` | `sync.Pool` |
| Lifecycle | Long-lived | Ephemeral (reset per use) |
| Pool size | Bounded (`MaxIdleConns`) | Unbounded, GC-managed |

---

## The Arc of Understanding

```
"logging is just fmt.Println"
        ↓
concurrent writes corrupt log lines → need mutex
        ↓
mutex covers format+write → serializes work that could be parallel
        ↓
move formatting outside mutex → need per-goroutine scratch buffers
        ↓
allocating buffers per log call → GC pressure under load
        ↓
sync.Pool → reuse buffers → lock-free per-P slots → victim cache for GC grace
        ↓
same pattern everywhere: HTTP buffers, JSON, databases, protobuf
        ↓
"logging is a microcosm of systems programming"
```

---

## Chapter 11: The Hidden Cost of Serialization

Understanding buffer pooling leads to a bigger realisation: **serialization is not free, and we introduce it constantly without thinking about it.**

Every log write serializes fields into JSON bytes. We studied this carefully — scratch buffers, GC pressure, mutex contention. But the exact same cost exists at every API boundary in a system:

```
Service A  →  marshal to JSON/protobuf  →  network  →  unmarshal  →  Service B
               ↑ scratch buffer                          ↑ scratch buffer
               ↑ GC pressure                             ↑ GC pressure
               ↑ CPU cycles building bytes               ↑ CPU cycles parsing bytes
```

Every microservice call pays this twice. Every database query pays it. Every cache read pays it. Every message queue publish/consume pays it.

### What serialization actually costs

At the code level it looks like one line:
```go
json.Marshal(myStruct)
```

What's actually happening:
- Allocate a scratch buffer
- Reflect over every field of the struct
- Convert each field to its string/byte representation
- Write delimiters, quotes, brackets
- Return the completed byte sequence
- GC eventually collects the scratch buffer

And then on the receiving end, unmarshal does the reverse — tokenize the bytes, allocate new struct fields, parse each value back into its Go type.

Under high concurrency, this is happening thousands of times per second across every service boundary.

### The invisible tax of microservices

In a monolith, a function call has zero serialization cost:

```go
result := orderService.GetOrder(id)  // in-process, just a pointer
```

Split that into two services:

```
GET /orders/{id}  →  marshal request  →  HTTP  →  unmarshal request
                                                 →  marshal response  →  HTTP  →  unmarshal response
```

Four serialization operations for what was zero. Every service boundary you add multiplies this cost across every request that crosses it.

Teams have measured 30-40% of total request latency in some microservice architectures being pure serialization overhead — not business logic, not database queries, not network RTT. Just converting data between bytes and structs.

### Why we don't notice

Frameworks hide it. `gin.ShouldBindJSON(&req)` looks like one line. gRPC stubs look like function calls. The cost is real but invisible until you profile under load and see serialization dominating your flame graph.

### The design implication

Every time you say "let's add an API" or "let's split this service," you're adding a serialization boundary. That's sometimes the right call — isolation, independent deployments, team ownership. But it's a **cost**, not just an architectural choice.

Questions worth asking before adding an API boundary:
- Can this be an in-process call instead?
- If it must be remote, can we use a binary format (protobuf) instead of JSON?
- Can we batch calls to amortize the serialization cost?
- Is the data structure designed to serialize efficiently (flat vs deeply nested)?

### Protobuf vs JSON — not just syntax

This is why protobuf exists. JSON serialization uses reflection and string manipulation. Protobuf uses generated code with direct field access — no reflection, smaller wire format, faster parse. The trade-off: schema must be defined upfront, less human-readable.

At high volume, this matters:
```
JSON:     ~500ns to marshal a typical request struct
Protobuf: ~100ns for the same struct
```

5x difference. Across millions of requests per day, that's real CPU time.

### The logging connection

This is what made logging non-trivial: it's serialization on every request, under concurrency, in the hot path. The solutions — zerolog, sync.Pool, narrow mutexes — are the same solutions high-performance serialization libraries use everywhere.

Logging just made the problem visible because it happens on **every single request**, not just at service boundaries.

---

## Key Takeaways

1. **A log write is format + write.** Format is parallelizable, write must be serialized. Putting both under the same mutex is the conservative but suboptimal choice.

2. **`sync.Pool` is for ephemeral scratch space.** Short-lived, frequently allocated, cheap to reset. Not for objects with meaningful state.

3. **Pool size is dynamic.** Grows under spikes, shrinks after two GC cycles. No manual sizing.

4. **Per-P slots make Get/Put lock-free in the common case.** The GC interaction (victim cache) gives a two-cycle grace period to smooth post-GC allocation spikes.

5. **Buffer pooling and connection pooling are the same idea.** Reuse expensive-to-create resources instead of creating them fresh per operation.

6. **The pattern appears everywhere in high-performance Go.** `net/http`, `encoding/json`, database drivers, protobuf — all use pooling internally.

7. **Serialization is not free — and we introduce it constantly.** Every API boundary pays serialization cost twice (marshal + unmarshal). Every microservice split multiplies this across every request that crosses the boundary. In-process function calls have zero serialization cost.

8. **"Let's add an API" is a serialization decision.** Before adding a service boundary, ask: can this be in-process? If remote is necessary, use binary formats (protobuf) over text (JSON), batch calls, and design flat data structures. 30-40% of request latency in some microservice architectures is pure serialization overhead — not business logic, not network RTT.

9. **Logging made serialization visible because it's on every single request.** The solutions (zerolog, sync.Pool, narrow mutexes) are the same patterns high-performance serialization libraries use everywhere. Logging is a microcosm of systems programming — it looks trivial on the surface, but when you dig in, it contains every major systems programming problem in miniature: concurrency, shared state, memory allocation, GC pressure, serialization cost, performance trade-offs.
