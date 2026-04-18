package progressstore

import (
	"context"
	"errors"
	"sync"
)

type MemStore struct {
	ms *memstore
}

type memstore struct {
	mu    sync.Mutex
	store map[string]Progress
}

var ups *memstore
var once sync.Once

func newMemstore() *memstore {
	once.Do(func() {
		ups = &memstore{mu: sync.Mutex{}, store: make(map[string]Progress)}
	})
	return ups
}

func NewMemStore() Store {
	return &MemStore{
		ms: newMemstore(),
	}
}

func (up *MemStore) GetProgressByID(ctx context.Context, id string) (Progress, error) {
	up.ms.mu.Lock()
	defer up.ms.mu.Unlock()
	if p, ok := up.ms.store[id]; ok {
		return p, nil
	}
	return Progress{}, errors.New("not found")
}

func (up *MemStore) DeleteProgressByID(ctx context.Context, id string) error {
	up.ms.mu.Lock()
	defer up.ms.mu.Unlock()
	delete(up.ms.store, id)
	return nil
}

func (up *MemStore) SetProgress(ctx context.Context, id string, p Progress) error {
	up.ms.mu.Lock()
	defer up.ms.mu.Unlock()
	up.ms.store[id] = p
	return nil
}
