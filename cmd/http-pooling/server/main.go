package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func Server() http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		// w.Header().Set("Connection", "keep-alive")
		// w.Header().Set("Connection", "close")
		defer r.Body.Close()
		s := strings.Builder{}
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			s.Write(sc.Bytes())
		}

		if s.String() == "slow" {
			time.Sleep(time.Second * 5)
		}

		json.NewEncoder(w).Encode(struct {
			Echo string `json:"echo"`
		}{
			Echo: s.String(),
		})
	})

	return http.Server{
		Addr:    ":4040",
		Handler: h2c.NewHandler(mux, &http2.Server{}),
	}
}

func main() {
	svr := Server()
	svr.ListenAndServe()
}
