package upload

import (
	"context"
	"fmt"
	"http-protocol-deep-dive/internal/apis"
	"net/http"
	"time"
)

// GET /upload/progress/{upload-id}
// uses SSE (Server-sent events)
func (u *UploadAPI) UploadProgress(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")

	id := r.PathValue("uploadid")

	for {
		select {
		case <-r.Context().Done():
			// if client closes connection
			return nil
		default:
			progress, err := u.ups.GetProgressByID(ctx, id)
			if err != nil {
				u.log.Error(ctx, "error getting progress from upload-progress-store", "err", err)
				return apis.NewError(http.StatusNotFound, "upload-id not found")
			}

			if progress.IsComplete {
				fmt.Fprintf(w, "event: upload-progress\ndata: {\"progress\": \"%v\", \"complete\": %v}\n\n", progress.SoFar, progress.IsComplete)
				u.ups.DeleteProgressByID(ctx, id)
				return nil
			}

			if progress.SoFar == 0 && !progress.IsComplete {
				time.Sleep(2000 * time.Millisecond)
				continue
			}
			if progress.Err != nil && !progress.IsComplete {
				fmt.Fprintf(w, "event: upload-progress\ndata: {\"error\": \"%v\"}\n\n", "failed to upload. Try again")
				return nil
			}

			fmt.Fprintf(w, "event: upload-progress\ndata: {\"progress\": \"%v\", \"complete\": %v}\n\n", progress.SoFar, progress.IsComplete)
			w.(http.Flusher).Flush()

			time.Sleep(200 * time.Millisecond)

		}

	}

}
