package upload

import (
	"http-protocol-deep-dive/internal/utilities/logger"
	"http-protocol-deep-dive/prototypes/01-file-upload/upload/uploadprogress"
)

type UploadAPI struct {
	log *logger.Logger
	ups *uploadprogress.Store
}

func New(log *logger.Logger, ups *uploadprogress.Store) *UploadAPI {
	return &UploadAPI{
		log: log,
		ups: ups,
	}
}
