package upload

import (
	"http-protocol-deep-dive/internal/utilities/logger"
	"http-protocol-deep-dive/prototypes/01-file-upload/upload/progressstore"
)

type UploadAPI struct {
	log *logger.Logger
	ups progressstore.Store
}

func New(log *logger.Logger, ups progressstore.Store) *UploadAPI {
	return &UploadAPI{
		log: log,
		ups: ups,
	}
}
