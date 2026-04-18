package router

import (
	"http-protocol-deep-dive/internal/middlewares/errors"
	"http-protocol-deep-dive/internal/middlewares/tracing"
	"http-protocol-deep-dive/internal/mux"
	"http-protocol-deep-dive/internal/utilities/logger"
	"http-protocol-deep-dive/prototypes/01-file-upload/upload"

	"github.com/go-redis/redis/v8"
)

func Router(rc *redis.Client, log *logger.Logger) *mux.Mux {
	errorsM := errors.ErrorMiddleware(log)
	traceM := tracing.TracingMiddleware(log)

	mux := mux.NewMux(log, errorsM, traceM)

	upload.Routes(mux, rc, log)

	return mux
}
