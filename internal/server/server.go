package server

import (
	"context"
	"http-protocol-deep-dive/internal/mux"
	"http-protocol-deep-dive/internal/utilities/logger"
	"net/http"
)

type Server struct {
	s   http.Server
	log *logger.Logger
}

func New(log *logger.Logger, addr string, mux *mux.Mux) Server {
	return Server{
		log: log,
		s: http.Server{
			Addr:    addr,
			Handler: mux,
		},
	}
}

func (s *Server) Start(ctx context.Context) error {
	s.log.Info(ctx, "starting server", "port", s.s.Addr)
	return s.s.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.log.Info(ctx, "shutting down server")
	return s.s.Shutdown(ctx)
}
