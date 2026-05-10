package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
)

func main() {

	port := flag.String("port", ":4040", "port number")
	flag.Parse()

	listener, err := net.Listen("tcp", *port)
	if err != nil {
		fmt.Printf("failed to listen on port %s -> %v\n", *port, err)
		return
	}
	log.Printf("listening on %s\n", *port)

	i := 0
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Printf("failed to accept connection: %v\n", err)
			// keep listening for new connections
			continue
		}

		go func(conn net.Conn, gri int) {
			defer conn.Close()
			sc := bufio.NewScanner(conn)

			who := conn.RemoteAddr()

			log.Printf("[go-routine-%d][%s]: connected\n", gri, who)

			for sc.Scan() {
				t := sc.Text()
				log.Printf("[go-routine-%d][%s]: %s\n", gri, who, t)
				fmt.Fprintf(conn, "[go-routine-%d]echo: %s\n", gri, t)
			}

			err := sc.Err()
			if err != nil {
				log.Printf("[go-routine-%d]err reading from connection: %v\n", gri, err)
				return
			}

			log.Printf("[go-routine-%d][%s]: disconnected\n", gri, who)

		}(conn, i)
		i++

	}

}
