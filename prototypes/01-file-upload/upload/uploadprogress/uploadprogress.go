package uploadprogress

import (
	"errors"
	"sync"
)

type Progress struct {
	Err        error
	Total      uint64
	SoFar      uint64
	IsComplete bool
}

type Store struct {
	ups *store
}

type store struct {
	mu    sync.Mutex
	store map[string]Progress
}

var ups *store
var once sync.Once

func new() *store {
	once.Do(func() {
		ups = &store{mu: sync.Mutex{}, store: make(map[string]Progress)}
	})
	return ups
}

// single ton pattern
func New() *Store {
	return &Store{
		ups: new(),
	}
}

func (up *Store) GetProgressById(id string) (Progress, error) {
	up.ups.mu.Lock()
	if p, ok := up.ups.store[id]; ok {
		up.ups.mu.Unlock()
		return p, nil
	}
	up.ups.mu.Unlock()
	return Progress{}, errors.New("not found")
}

func (up *Store) DeleteProgressById(id string) error {
	up.ups.mu.Lock()
	delete(up.ups.store, id)
	up.ups.mu.Unlock()
	return nil
}

func (up *Store) SetProgress(id string, p Progress) error {
	up.ups.mu.Lock()
	up.ups.store[id] = p
	up.ups.mu.Unlock()
	return nil
}
