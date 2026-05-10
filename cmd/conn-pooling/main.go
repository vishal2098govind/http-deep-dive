package main

import (
	"bufio"
	"fmt"
	"http-protocol-deep-dive/cmd/conn-pooling/pool"
	"log"
	"net"
	"sync"
	"time"
)

func main() {

	hosts := []string{
		":3030",
		":4040",
		":5050",
	}

	pool := pool.NewSharedConnPool()
	log.SetFlags(log.Lmicroseconds)

	wg := sync.WaitGroup{}
	round := 1
	for _, addr := range hosts {
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(i int, addr string) {
				defer wg.Done()
				conn, err := pool.GetConn(round, i, addr)
				if err != nil {
					return
				}
				log.Printf("[round-%d][go-routine-%s-%d] using conn.LocalAddress(%v)", round, addr, i, conn.LocalAddr())

				fmt.Fprintf(conn, "[%d]: hi\n", i)
				time.Sleep(time.Second * 1)

				sc := bufio.NewScanner(conn)
				sc.Scan()
				t := sc.Text()
				log.Printf("[round-%d][response-from-server] for [go-routine-%s-%d][who-%v]:%s\n", round, addr, i, conn.LocalAddr(), t)
				pool.ReturnConn(round, addr, conn)
			}(i, addr)
		}
	}

	wg.Wait()
	round++

	for _, addr := range hosts {
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func(i int, addr string) {
				defer wg.Done()
				conn, err := pool.GetConn(round, i, addr)
				if err != nil {
					return
				}
				log.Printf("[round-%d][go-routine-%s-%d] using conn.LocalAddress(%v)", round, addr, i, conn.LocalAddr())

				fmt.Fprintf(conn, "[%d]: hi\n", i)
				time.Sleep(time.Second * 1)

				sc := bufio.NewScanner(conn)
				sc.Scan()
				t := sc.Text()
				log.Printf("[round-%d][response-from-server] for [go-routine-%s-%d][who-%v]:%s\n", round, addr, i, conn.LocalAddr(), t)
				pool.ReturnConn(round, addr, conn)
			}(i, addr)
		}
	}

	wg.Wait()

}

func runDedicatedPool() {
	pool := pool.New(":4040")

	wg := sync.WaitGroup{}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := pool.GetConn(i)
			log.Printf("[go-routine-%d] using conn.LocalAddress(%v)\n", i, conn.LocalAddr())
			if err != nil {
				return
			}

			// Writer
			fmt.Fprintf(conn, "[%d]: hi\n", i)

			time.Sleep(time.Second * 2)

			sc := bufio.NewScanner(conn)
			sc.Scan()
			t := sc.Text()
			fmt.Printf("[response-from-server]:%s\n", t)
			pool.ReturnConn(conn)
		}(i)
	}

	wg.Wait()
}

func run() {
	pool := pool.New(":4040")
	conn3, err := pool.GetConn(0)
	if err != nil {
		return
	}
	conn4, err := pool.GetConn(0)
	if err != nil {
		return
	}

	// warming up pool
	conns := []net.Conn{}
	for i := 0; i < 3; i++ {
		conn, err := pool.GetConn(0)
		if err != nil {
			continue
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		pool.ReturnConn(conn)
	}

	conns = []net.Conn{}
	for i := 0; i < 3; i++ {
		conn, err := pool.GetConn(0)
		if err != nil {
			fmt.Printf("failed to get conn from pool: %v\n", err)
			return
		}
		conns = append(conns, conn)
	}
	for i, conn := range conns {
		handleConn(i, conn, pool)
	}

	conns = []net.Conn{}
	for i := 0; i < 3; i++ {
		conn, err := pool.GetConn(0)
		if err != nil {
			fmt.Printf("failed to get conn from pool: %v\n", err)
			return
		}
		conns = append(conns, conn)
	}
	for i, conn := range conns {
		handleConn(i, conn, pool)
	}

	handleConn(3, conn3, pool)
	handleConn(4, conn4, pool)

	time.Sleep(time.Second * 60)
}

func handleConn(i int, conn net.Conn, pool *pool.ConnPool) {
	fmt.Printf("iter[%d]: conn.LocalAddress(%v)\n", i, conn.LocalAddr())

	// Writer
	fmt.Fprintf(conn, "[%d]: hello, stay alive from\n", i)

	// Reader
	sc := bufio.NewScanner(conn)
	sc.Scan()
	t := sc.Text()
	fmt.Println(t)
	pool.ReturnConn(conn)
}
