package mux

import (
	"context"
	"fmt"
	"http-protocol-deep-dive/internal/utilities/logger"
	"net/http"
)

type Handler func(context.Context, http.ResponseWriter, *http.Request) error

type Mux struct {
	mux  *http.ServeMux
	log  *logger.Logger
	mids []MiddlewareFunc
}

// Middlewares are evaluated right to left.
// The rightmost middleware is closest to the handler (most qualified).
//
// Example: NewMux(log, m1, m2) → m2 runs first then m1.
// think of it like a middleware nearer to handler will be evaluated more closer in time to when the handler itself will be evaluated
// u open the chain of boxes from outer most till u reach inner most box which is the handler
func NewMux(log *logger.Logger, mids ...MiddlewareFunc) *Mux {
	return &Mux{
		mux:  http.NewServeMux(),
		log:  log,
		mids: mids,
	}
}

func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mux.ServeHTTP(w, r)
}

// Middlewares are evaluated right to left.
// The rightmost middleware is closest to the handler (most qualified).
//
// Example: HandleFunc("/resource", h, authz, authn) → authn runs first to make the request bit more capable to reach h, and then authz runs to make request even more capable to reach h as the request is reaching nearer to h than how far it was while it was at authn.
//
// think of it like a middleware nearer to handler will be evaluated more closer in time to when the handler itself will be evaluated
// u open the chain of boxes from outer most till u reach inner most box which is the handler
func (m *Mux) HandleFunc(pattern string, handler Handler, mids ...MiddlewareFunc) {
	m.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		m.handleFunc(w, r, handler, mids...)
	})
}

func (m *Mux) handleFunc(w http.ResponseWriter, r *http.Request, h Handler, mids ...MiddlewareFunc) {
	ctx := r.Context()

	h = AddMiddlewares(h, mids...)
	h = AddMiddlewares(h, m.mids...)

	err := h(ctx, w, r)
	if err != nil {
		fmt.Printf("something went wrong: %v\n", err)
	}

}
