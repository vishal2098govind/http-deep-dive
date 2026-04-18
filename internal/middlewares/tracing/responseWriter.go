package tracing

import (
	"net/http"
)

type tracingResponseWriter struct {
	w          http.ResponseWriter
	StatusCode int
}

func newTracingResponseWriter(w http.ResponseWriter) *tracingResponseWriter {
	return &tracingResponseWriter{w: w}
}

func (w *tracingResponseWriter) WriteHeader(statusCode int) {
	w.StatusCode = statusCode
	w.w.WriteHeader(statusCode)
}

func (w *tracingResponseWriter) Header() http.Header {
	return w.w.Header()
}

func (w *tracingResponseWriter) Write(b []byte) (int, error) {
	if w.StatusCode == 0 {
		w.StatusCode = http.StatusOK
	}
	return w.w.Write(b)
}

func (w *tracingResponseWriter) Flush() {
	if f, ok := w.w.(http.Flusher); ok {
		f.Flush()
	}
}
