package main

import (
	"bufio"
	"context"
	"log"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

func Client() http.Client {
	return http.Client{

		Transport: &http.Transport{
			MaxIdleConnsPerHost: 3,
			MaxIdleConns:        5,
			MaxConnsPerHost:     8,
			IdleConnTimeout:     time.Duration(10 * time.Second),
		},
	}
}

func main() {
	client := Client()
	sg := sync.WaitGroup{}
	trace := httptrace.ClientTrace{
		ConnectStart: func(network, addr string) {
			log.Printf("ConnectStart: %s:%s", network, addr)
		},
		GotConn: func(gci httptrace.GotConnInfo) {
			log.Printf("GotConn: conn.LocalAddress(%s) for conn.RemoteAddress(%s)", gci.Conn.LocalAddr(), gci.Conn.RemoteAddr())
		},
	}
	ctx := httptrace.WithClientTrace(context.Background(), &trace)

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
}
