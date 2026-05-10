package pool

import (
	"log"
	"net"
	"sync"
	"time"
)

type SharedConnPool struct {
	mu   sync.Mutex
	pool map[string][]struct {
		idleSince time.Time
		conn      net.Conn
	}

	MaxIdleConnsPerHost int // per host idle-conn-cap knob

	// global idle-conn-cap knob
	// we need global knob for idle-conn to avoid resource (fds in RAM) exhaustion
	MaxIdleConns    int
	IdleConns       chan struct{} // semaphore
	IdleConnTimeout time.Duration

	// to avoid overwhelming server and allowing burst
	// there's no global max-conns knob - since it doesn't really avoid make sense
	// we either want to serve burst per host or want to avoid overwhelming a host
	// we don't want to serve overall burst or avoid  overwhelming overall all hosts, doesn't make any sense
	MaxConns        map[string]chan struct{}
	MaxConnsPerHost int
}

func NewSharedConnPool() *SharedConnPool {
	maxIdleConnsPerHost := 3
	maxIdleConns := 5
	maxConnsPerHost := 8

	idleConns := make(chan struct{}, maxIdleConns)
	for i := 0; i < maxIdleConns; i++ {
		idleConns <- struct{}{}
	}

	// const idleConnTimeout = 60

	const idleConnTimeout = 5 // for testing

	pool := &SharedConnPool{
		pool: make(map[string][]struct {
			idleSince time.Time
			conn      net.Conn
		}),
		IdleConnTimeout:     time.Second * idleConnTimeout,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		MaxIdleConns:        maxIdleConns,
		IdleConns:           idleConns,
		MaxConnsPerHost:     maxConnsPerHost,
		MaxConns:            make(map[string]chan struct{}),
	}

	go evictSharedIdleTimedoutConns(pool)

	return pool
}

func evictSharedIdleTimedoutConns(pool *SharedConnPool) {

	for {
		time.Sleep(pool.IdleConnTimeout / 2)

		timedOutConns := []net.Conn{}
		pool.mu.Lock()

		for addr := range pool.pool {

			// for each host, find timed out connections from the frontend of the queue
			for {
				if len(pool.pool[addr]) == 0 {
					break
				}

				c := pool.pool[addr][0]
				if time.Since(c.idleSince) > pool.IdleConnTimeout {
					pool.MaxConns[addr] <- struct{}{}
					pool.pool[addr] = pool.pool[addr][1:]
					pool.IdleConns <- struct{}{}
					timedOutConns = append(timedOutConns, c.conn)
				} else {
					break
				}
			}
		}

		pool.mu.Unlock()

		for _, v := range timedOutConns {
			v.Close()
		}
	}
}

func (p *SharedConnPool) GetConn(round, i int, addr string) (net.Conn, error) {
	p.mu.Lock()
	pool, ok := p.pool[addr]
	if !ok || len(pool) == 0 {
		// new host

		mc, ok := p.MaxConns[addr]
		if !ok {
			p.MaxConns[addr] = make(chan struct{}, p.MaxConnsPerHost)
			for i := 0; i < p.MaxConnsPerHost; i++ {
				p.MaxConns[addr] <- struct{}{}
			}
			mc = p.MaxConns[addr]
		}
		p.mu.Unlock()

		select {
		case <-mc:
		default:
			log.Printf("[round-%d][go-routine-%s-%d] blocking for MaxConnsPerHost\n", round, addr, i)
			<-mc
		}
		return net.Dial("tcp", addr)
	}

	conn := pool[0].conn
	p.pool[addr] = p.pool[addr][1:]
	log.Printf("[round-%d] [go-routine-%s-%d] using conn (%v) from pool and releasing global IdleConns", round, addr, i, conn.LocalAddr())
	p.mu.Unlock()

	// release one global IdleConn
	// ideally this should never get blocked because
	// if this line of code is reached, it means pool has idle conns,
	// if a pool has idle conns, it means conns must have been returned to pool via ReturnConn
	// and ReturnConn consumes one global IdleConn always, thus it will never be full
	// thus, never blocked here
	p.IdleConns <- struct{}{}

	return conn, nil
}

func (p *SharedConnPool) ReturnConn(round int, addr string, conn net.Conn) {
	p.mu.Lock()

	pool, ok := p.pool[addr]
	if ok && len(pool) == p.MaxIdleConnsPerHost {
		log.Printf("[round-%d] reached max-idle for this host(%s), releasing conn to MaxConnsPerHost and closing conn: %v", round, addr, conn.RemoteAddr())
		conn.Close()
		p.MaxConns[addr] <- struct{}{}
		p.mu.Unlock()
		return
	}

	defer p.mu.Unlock()

	select {
	// if able to consume one global IdleConns
	case <-p.IdleConns:
		pool = append(pool, struct {
			idleSince time.Time
			conn      net.Conn
		}{
			idleSince: time.Now(),
			conn:      conn,
		})
		log.Printf("[round-%d] returning conn %v to pool", round, addr)
		p.pool[addr] = pool

	// else, global IdleConns is full
	// close conn and release on MaxConns for that host
	default:
		// release one conn to MaxConnsPerHost
		log.Printf("[round-%d] [pool-global-idle-cap] releasing the conn to MaxConnsPerHost and closing conn: %v", round, conn.RemoteAddr())
		conn.Close()
		p.MaxConns[addr] <- struct{}{}
	}
}
