package main

import (
	"context"
	"http-protocol-deep-dive/internal/server"
	"http-protocol-deep-dive/internal/utilities/logger"
	"http-protocol-deep-dive/prototypes/router"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx := context.Background()

	wd, _ := os.Getwd()

	log := logger.New(os.Stdout, wd)

	mux := router.Router(log)

	addr := "0.0.0.0:8080"

	s := server.New(log, addr, mux)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	serverErr := make(chan error, 1)

	go func() {
		err := s.Start(ctx)
		if err != nil {
			log.Error(ctx, "failed starting server", "err", err)
		}
		serverErr <- err
	}()

	select {
	case err := <-serverErr:
		log.Error(ctx, "failed to start server", "err", err)
	case <-sig:
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			log.Error(ctx, "shutting down error", "err", err)
		}
	}
}
