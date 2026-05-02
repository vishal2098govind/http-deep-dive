package main

import (
	"bufio"
	"fmt"
	"net"
)

func main() {

	listener, err := net.Listen("tcp", ":4040")
	if err != nil {
		fmt.Printf("failed to listen on port :4040 -> %v\n", err)
		return
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Printf("failed to accept connection: %v\n", err)
			// keep listening for new connections
			continue
		}

		go func(conn net.Conn) {
			defer conn.Close()
			sc := bufio.NewScanner(conn)

			who := conn.RemoteAddr()

			fmt.Printf("[%s]: connected\n", who)

			for sc.Scan() {
				t := sc.Text()
				fmt.Printf("[%s]: %s\n", who, t)
				fmt.Fprintf(conn, "echo: %s\n", t)
			}

			err := sc.Err()
			if err != nil {
				fmt.Printf("err reading from connection: %v\n", err)
				return
			}

			fmt.Printf("[%s]: disconnected\n", who)

		}(conn)

	}

}
