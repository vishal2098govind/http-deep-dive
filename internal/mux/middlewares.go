package mux

type MiddlewareFunc func(h Handler) Handler

func AddMiddlewares(h Handler, m ...MiddlewareFunc) Handler {
	for _, v := range m {
		h = v(h)
	}

	return h
}
