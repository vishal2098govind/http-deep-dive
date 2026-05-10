package pool

import (
	"log"
	"net"
	"sync"
	"time"
)

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
	// for handling burst requests
	MaxConnsPerHost := 6

	// for avoiding many stale connections in pool
	MaxIdleConnsPerHost := 3

	// IdleConnTimeout := 60
	IdleConnTimeout := 5 // for testing

	maxConnsPerHost := make(chan struct{}, MaxConnsPerHost)
	for i := 0; i < MaxConnsPerHost; i++ {
		maxConnsPerHost <- struct{}{}
	}

	pool := &ConnPool{
		pool: []struct {
			idleSince time.Time
			conn      net.Conn
		}{},
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
				p.MaxConnsPerHost <- struct{}{}
				if len(p.pool) == 1 {
					p.pool = []struct {
						idleSince time.Time
						conn      net.Conn
					}{}
					break
				}
				newPool := p.pool[1:]
				p.pool = newPool
			} else {
				break
			}
		}

		p.mu.Unlock()
	}

}

func (p *ConnPool) GetConn(i int) (net.Conn, error) {

	p.mu.Lock()
	if len(p.pool) == 0 {
		p.mu.Unlock()
		// <-p.MaxConnsPerHost
		// to be able to log before getting blocked.
		select {
		case <-p.MaxConnsPerHost:
		default:
			log.Printf("[go-routine-%d] blocking for MaxConnsPerHost", i)
			<-p.MaxConnsPerHost

		}
		return net.Dial("tcp", p.Addr)
	}

	defer p.mu.Unlock()
	conn := p.pool[0].conn
	p.pool = p.pool[1:]
	return conn, nil

	// select {
	// case conn := <-p.pool:
	// 	// usually connection staleness is left to the caller to handle so that we do not do any destructive read if the connection is still not stale yet
	// 	// conn.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
	// 	// buff := make([]byte, 1)
	// 	// _, err := conn.Read(buff)
	// 	// if err == io.EOF {
	// 	// 	return net.Dial("tcp", p.Addr)
	// 	// }
	// 	return conn, nil
	// default:
	// 	return net.Dial("tcp", p.Addr)
	// }

}

func (p *ConnPool) ReturnConn(c net.Conn) {

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.pool) == p.MaxIdleConnsPerHost {
		// opportunistic idle-conn-eviction
		if time.Since(p.pool[0].idleSince) >= p.IdleConnTimeout {
			p.pool[0].conn.Close()
			p.MaxConnsPerHost <- struct{}{}
			newPool := []struct {
				idleSince time.Time
				conn      net.Conn
			}{}
			newPool = append(newPool, p.pool[1:]...)
			newPool = append(newPool, struct {
				idleSince time.Time
				conn      net.Conn
			}{conn: c, idleSince: time.Now()})

			p.pool = newPool
			return
		}
		c.Close()
		p.MaxConnsPerHost <- struct{}{}
		return
	}

	p.pool = append(p.pool, struct {
		idleSince time.Time
		conn      net.Conn
	}{
		idleSince: time.Now(),
		conn:      c,
	})

	// select {
	// case p.pool <- c:
	// default:
	// 	c.Close()
	// }

	// p.mu.Lock()
	// defer p.mu.Unlock()
	// if len(p.pool) == p.MaxConn {
	// 	c.Close()
	// 	return
	// }

	// p.pool <- c
}
