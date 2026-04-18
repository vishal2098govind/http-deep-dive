package router

import (
	"http-protocol-deep-dive/internal/middlewares/errors"
	"http-protocol-deep-dive/internal/middlewares/tracing"
	"http-protocol-deep-dive/internal/mux"
	"http-protocol-deep-dive/internal/utilities/logger"
	"http-protocol-deep-dive/prototypes/01-file-upload/upload"
)

func Router(log *logger.Logger) *mux.Mux {
	errorsM := errors.ErrorMiddleware(log)
	traceM := tracing.TracingMiddleware(log)

	mux := mux.NewMux(log, errorsM, traceM)

	upload.Routes(mux, log)

	return mux
}
