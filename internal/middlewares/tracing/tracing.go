package tracing

import (
	"context"
	"http-protocol-deep-dive/internal/mux"
	"http-protocol-deep-dive/internal/tracing"

	"http-protocol-deep-dive/internal/utilities/logger"
	"net/http"
	"time"

	"github.com/google/uuid"
)

func TracingMiddleware(log *logger.Logger) mux.MiddlewareFunc {
	return func(h mux.Handler) mux.Handler {
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {

			traceId := uuid.NewString()
			ctx = tracing.WithTrace(ctx, tracing.Trace{
				Id:              traceId,
				RequestMethod:   r.Method,
				RequestEndpoint: r.RequestURI,
			})

			log.Info(ctx, "Request started")

			t := time.Now()
			wr := newTracingResponseWriter(w)

			err := h(ctx, wr, r)
			log.Info(ctx, "Request ended. request took", "duration", time.Since(t).String(), "status", wr.StatusCode)

			if err != nil {
				return err
			}

			return nil
		}
	}
}
