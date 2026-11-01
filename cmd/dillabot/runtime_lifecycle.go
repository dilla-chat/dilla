package main

import (
	"sync"

	"github.com/jonasthim/dilla/internal/id"
)

// installationLifecycle serializes changes to one installation while leaving
// registry lookups and unrelated installations free to progress.
type installationLifecycle struct {
	mu         sync.Mutex
	operations map[id.ID]*installationOperation
}

type installationOperation struct {
	mu    sync.Mutex
	users int
}

func (l *installationLifecycle) operation(installation id.ID) (*installationOperation, func()) {
	l.mu.Lock()
	if l.operations == nil {
		l.operations = make(map[id.ID]*installationOperation)
	}
	op := l.operations[installation]
	if op == nil {
		op = &installationOperation{}
		l.operations[installation] = op
	}
	op.users++
	l.mu.Unlock()
	return op, func() {
		l.mu.Lock()
		op.users--
		if op.users == 0 {
			delete(l.operations, installation)
		}
		l.mu.Unlock()
	}
}

func (l *installationLifecycle) lock(installation id.ID) func() {
	op, release := l.operation(installation)
	op.mu.Lock()
	return func() {
		op.mu.Unlock()
		release()
	}
}

func (l *installationLifecycle) tryLock(installation id.ID) (func(), bool) {
	op, release := l.operation(installation)
	if !op.mu.TryLock() {
		release()
		return nil, false
	}
	return func() {
		op.mu.Unlock()
		release()
	}, true
}
