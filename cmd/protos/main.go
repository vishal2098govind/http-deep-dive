package main

import (
	"context"
	"flag"
	"fmt"
	"http-protocol-deep-dive/internal/server"
	"http-protocol-deep-dive/internal/utilities/logger"
	"http-protocol-deep-dive/prototypes/router"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	"go.yaml.in/yaml/v3"
)

type Config struct {
	Protos struct {
		Hostname string `yaml:"hostname"`
		Hostport int    `yaml:"hostport"`
	} `yaml:"protos"`
	Redis struct {
		Hostname string `yaml:"hostname"`
		Hostport int    `yaml:"hostport"`
	} `yaml:"redis"`
}

func main() {
	ctx := context.Background()

	wd, _ := os.Getwd()

	log := logger.New(os.Stdout, wd)

	cfg := flag.String("config", "cmd/protos/config/local.yaml", "path to config file")
	flag.Parse()

	b, err := os.ReadFile(*cfg)
	if err != nil {
		log.Error(ctx, "failed to read config file", "err", err)
		os.Exit(1)
		return
	}
	var conf Config
	err = yaml.Unmarshal(b, &conf)
	if err != nil {
		log.Error(ctx, "failed to unmarshal config yaml", "err", err)
		os.Exit(1)
		return
	}
	log.Info(ctx, "using config", "config", conf)

	rc := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", conf.Redis.Hostname, conf.Redis.Hostport),
	})

	mux := router.Router(rc, log)

	addr := fmt.Sprintf("%s:%d", conf.Protos.Hostname, conf.Protos.Hostport)

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
		os.Exit(1)
	case <-sig:
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			log.Error(ctx, "shutting down error", "err", err)
		}
	}
}
