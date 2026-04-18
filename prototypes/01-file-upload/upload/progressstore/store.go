package progressstore

import "context"

type Store interface {
	DeleteProgressByID(ctx context.Context, id string) error
	GetProgressByID(ctx context.Context, id string) (Progress, error)
	SetProgress(ctx context.Context, id string, p Progress) error
}
