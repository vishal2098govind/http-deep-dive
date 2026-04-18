package upload

import (
	"context"
	"fmt"
	"http-protocol-deep-dive/internal/apis"
	"http-protocol-deep-dive/internal/ratelimit/slowreader"
	"http-protocol-deep-dive/prototypes/01-file-upload/upload/uploadprogress"
	"http-protocol-deep-dive/prototypes/01-file-upload/upload/uploadwriter"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
)

// PUT /upload/{uploadid}
func (u *UploadAPI) UploadFile(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	uploadid := r.PathValue("uploadid")
	_, err := u.ups.GetProgressById(uploadid)
	if err != nil {
		return apis.NewError(http.StatusBadRequest, "upload id not found")
	}
	ctype := r.Header.Get("content-type")
	mtype, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		return apis.NewErrorf(http.StatusBadRequest, "invalid content type. expected multiplart/form-data", "content-type sent in request: %s", ctype)
	}

	if mtype != "multipart/form-data" {
		return apis.NewError(http.StatusBadRequest, "invalid content type. expected multipart/form-data")
	}

	_, ok := params["boundary"]
	if !ok {
		return apis.NewErrorf(http.StatusBadRequest, "boundary not found in multipart/form-data content type", "content-type sent in request: %s", ctype)
	}

	reader, err := r.MultipartReader()
	if err != nil {
		u.log.Error(ctx, "failed to get multiplart reader", "action", "r.MultipartReader", "err", err)
		return apis.NewError(http.StatusInternalServerError, "something went wrong. try again.")
	}

	filePartFound := false

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			u.log.Error(ctx, "error reading next part", "action", "reader.NextPart", "err", err)
			return apis.NewError(http.StatusInternalServerError, "failed to read part")
		}
		if part != nil {
			ctype := part.Header.Get("content-type")

			_, _, err := mime.ParseMediaType(ctype)
			if err != nil {
				u.log.Error(ctx, "UploadFile", "action", fmt.Sprintf("part.mime.ParseMediaType(%s)", ctype), "err", err)
				return apis.NewErrorf(http.StatusBadRequest, "invalid content type in one of the parts in multipart/form-data", "content-type sent in request: %s", ctype)
			}

			cdtype := part.Header.Get("content-disposition")
			cdmtype, cdparams, err := mime.ParseMediaType(cdtype)
			if err != nil {
				u.log.Error(ctx, "UploadFile", "action", fmt.Sprintf("part.mime.ParseMediaType(%s)", cdtype), "err", err)
				return apis.NewErrorf(http.StatusBadRequest, "invalid content disposition in one of the parts in multipart/form-data", "content-disposition sent in request: %s", cdtype)
			}

			if cdmtype != "form-data" {
				return apis.NewErrorf(http.StatusBadRequest, "invalid content disposition in one of the parts in multipart/form-data", "content-disposition sent in request: %s", cdmtype)
			}

			// partLen := part.Header.Get("content-length")
			// u.log.Debug(ctx, "part.content-length", "partLen", partLen)

			filename, ok := cdparams["filename"]

			if ok {
				filePartFound = true
				basepath := filepath.Base(filename)
				tmp, err := os.Create(fmt.Sprintf("./%s", basepath))
				if err != nil {
					u.log.Error(ctx, "UploadFile", "action", "os.Create(./uploaded-file)", "err", err)
					return apis.NewError(http.StatusBadRequest, "something went wrong")
				}
				defer tmp.Close()

				u.ups.SetProgress(uploadid, uploadprogress.Progress{})
				wr := uploadwriter.New(tmp, uploadid, u.ups)
				n, err := io.Copy(wr, slowreader.New(part))
				u.ups.SetProgress(uploadid, uploadprogress.Progress{
					Err:        err,
					Total:      uint64(n),
					SoFar:      uint64(n),
					IsComplete: true,
				})
				if err != nil {
					u.log.Error(ctx, "UploadFile", "action", "io.Copy(tmp, part)", "err", err)
					return apis.NewError(http.StatusBadRequest, "something went wrong")
				}
			}

		}

	}

	if !filePartFound {
		return apis.NewError(http.StatusBadRequest, "file part not found in request")
	}

	apis.WriteJson(w, http.StatusOK, apis.ApiResponse{
		Message: "file uploaded",
	})

	return nil
}
