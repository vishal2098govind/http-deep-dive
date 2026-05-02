package main

import (
	"fmt"
	"io"
	"net"
	"os"
)

type WrappedReader struct {
	r io.Reader
}

func (wr WrappedReader) Read(p []byte) (int, error) {
	n, err := wr.r.Read(p)
	s := string(p[:n])
	if s == "EOF\n" {
		return 0, io.EOF
	}
	return n, err
}

func main() {
	conn, err := net.Dial("tcp", ":4040")
	if err != nil {
		fmt.Printf("failed to connect to server: %v\n", err)
		return
	}
	fmt.Println("connected to server")

	done := make(chan any, 1)

	// writer
	go func() {
		io.Copy(conn, WrappedReader{r: os.Stdin})
		conn.(*net.TCPConn).CloseWrite()
	}()

	// reader
	go func() {
		io.Copy(os.Stdout, conn)
		done <- 1
	}()

	<-done

	conn.Close()
}
