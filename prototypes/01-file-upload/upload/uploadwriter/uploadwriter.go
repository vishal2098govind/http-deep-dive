package uploadwriter

import (
	"context"
	"http-protocol-deep-dive/prototypes/01-file-upload/upload/progressstore"
	"io"
)

type Writer struct {
	w     io.Writer
	id    string
	sofar uint64
	ups   progressstore.Store
	ctx   context.Context
}

func New(ctx context.Context, w io.Writer, id string, ups progressstore.Store) *Writer {
	return &Writer{
		w:     w,
		id:    id,
		sofar: 0,
		ups:   ups,
		ctx:   ctx,
	}
}

func (uw *Writer) Write(p []byte) (int, error) {
	n, err := uw.w.Write(p)
	uw.sofar += uint64(n)
	uw.ups.SetProgress(uw.ctx, uw.id, progressstore.Progress{
		Err:   err,
		SoFar: uw.sofar,
	})
	return n, err
}
