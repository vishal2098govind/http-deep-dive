package uploadwriter

import (
	"http-protocol-deep-dive/prototypes/01-file-upload/upload/uploadprogress"
	"io"
)

type Writer struct {
	w     io.Writer
	id    string
	sofar uint64
	ups   *uploadprogress.Store
}

func New(w io.Writer, id string, ups *uploadprogress.Store) *Writer {
	return &Writer{
		w:     w,
		id:    id,
		sofar: 0,
		ups:   ups,
	}
}

func (uw *Writer) Write(p []byte) (int, error) {
	n, err := uw.w.Write(p)
	uw.sofar += uint64(n)
	uw.ups.SetProgress(uw.id, uploadprogress.Progress{
		Err:   err,
		SoFar: uw.sofar,
	})
	return n, err
}
