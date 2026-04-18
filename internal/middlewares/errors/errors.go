package errors

import (
	"context"
	"errors"
	"http-protocol-deep-dive/internal/apis"
	"http-protocol-deep-dive/internal/mux"
	"http-protocol-deep-dive/internal/utilities/logger"
	"net/http"
)

func ErrorMiddleware(log *logger.Logger) mux.MiddlewareFunc {
	return func(h mux.Handler) mux.Handler {
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {

			err := h(ctx, w, r)

			if err == nil {
				return nil
			}

			log.Error(ctx, "captured error in error-middleware", "err", err)

			var apiError apis.Error
			if errors.As(err, &apiError) {
				apis.WriteJson(w, apiError.StatusCode, apis.ApiResponse{
					Message: apiError.Message,
				})
				return nil
			}

			apis.WriteJson(w, http.StatusInternalServerError, apis.ApiResponse{
				Message: err.Error(),
			})

			return nil
		}
	}
}
