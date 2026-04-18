package progressstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-redis/redis/v8"
)

type RedisStore struct {
	rclient *redis.Client
}

// DeleteProgressByID implements [Store].
func (r *RedisStore) DeleteProgressByID(ctx context.Context, id string) error {
	_, err := r.rclient.Del(ctx, r.key(id)).Result()
	if err != nil {
		return fmt.Errorf("redis.Del: %w", err)
	}
	return nil
}

// GetProgressByID implements [Store].
func (r *RedisStore) GetProgressByID(ctx context.Context, id string) (Progress, error) {
	res, err := r.rclient.HGetAll(ctx, r.key(id)).Result()
	if err != nil {
		return Progress{}, fmt.Errorf("redis.HGetAll:%w", err)
	}
	if len(res) == 0 {
		return Progress{}, errors.New("not found")
	}
	return fromMap(res)
}

func (r *RedisStore) key(id string) string {
	return fmt.Sprintf("upload:progress:%s", id)
}

// SetProgress implements [Store].
func (r *RedisStore) SetProgress(ctx context.Context, id string, p Progress) error {
	_, err := r.rclient.HSet(ctx, r.key(id), p.fields()).Result()
	if err != nil {
		return fmt.Errorf("redis.HSet: %w", err)
	}
	return nil
}

func NewRedisStore(rc *redis.Client) Store {
	return &RedisStore{
		rclient: rc,
	}
}
