package upload

import (
	"context"
	"fmt"
	"http-protocol-deep-dive/internal/apis"
	"http-protocol-deep-dive/prototypes/01-file-upload/upload/progressstore"
	"net/http"

	"github.com/google/uuid"
)

// POST /upload/initiate
func (u *UploadAPI) InitiateUpload(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	id := uuid.NewString()
	u.ups.SetProgress(ctx, id, progressstore.Progress{})
	apis.WriteJson(w, http.StatusOK, apis.ApiResponse{
		Data: struct {
			UploadId  string `json:"upload_id"`
			UploadUrl string `json:"upload_url"`
		}{
			UploadId:  id,
			UploadUrl: fmt.Sprintf("/upload/%v", id),
		},
	})
	return nil
}
