package upload

import (
	"http-protocol-deep-dive/internal/mux"
	"http-protocol-deep-dive/internal/utilities/logger"
	"http-protocol-deep-dive/prototypes/01-file-upload/upload/uploadprogress"
)

func Routes(mux *mux.Mux, log *logger.Logger) {
	ups := uploadprogress.New()
	api := New(log, ups)

	mux.HandleFunc("POST /upload/initiate", api.InitiateUpload)
	mux.HandleFunc("PUT /upload/{uploadid}", api.UploadFile)
	mux.HandleFunc("GET /upload/progress/{uploadid}", api.UploadProgress)
}
