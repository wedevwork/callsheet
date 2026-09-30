package workspace

import (
	"context"
	"sync"
)

// ctxMutex is a mutual-exclusion lock whose acquisition can be cancelled
// (the manager's registry lock).
type ctxMutex struct{ ch chan struct{} }

func newCtxMutex() *ctxMutex { return &ctxMutex{ch: make(chan struct{}, 1)} }

// Lock acquires the lock unless ctx ends first; a context that has already
// ended never acquires it.
func (m *ctxMutex) Lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock releases the lock.
func (m *ctxMutex) Unlock() { <-m.ch }

// rwLock is a writer-preferring readers/writer lock whose acquisitions can
// be cancelled (each workspace's lock). A waiting writer blocks new
// readers, so a stream of fetches cannot starve a push, prune or removal.
type rwLock struct {
	mu      sync.Mutex
	readers int
	writer  bool
	waiting int
	wake    chan struct{}
}

// signal wakes every waiter to recheck (mu held).
func (l *rwLock) signal() {
	if l.wake != nil {
		close(l.wake)
		l.wake = nil
	}
}

// waitCh returns the current wake channel (mu held).
func (l *rwLock) waitCh() chan struct{} {
	if l.wake == nil {
		l.wake = make(chan struct{})
	}
	return l.wake
}

// RLock acquires a shared lock unless ctx ends first.
func (l *rwLock) RLock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	for l.writer || l.waiting > 0 {
		w := l.waitCh()
		l.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			return ctx.Err()
		}
		l.mu.Lock()
	}
	l.readers++
	l.mu.Unlock()
	return nil
}

// RUnlock releases a shared lock.
func (l *rwLock) RUnlock() {
	l.mu.Lock()
	l.readers--
	if l.readers == 0 {
		l.signal()
	}
	l.mu.Unlock()
}

// Lock acquires the exclusive lock unless ctx ends first.
func (l *rwLock) Lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	l.waiting++
	for l.writer || l.readers > 0 {
		w := l.waitCh()
		l.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			l.mu.Lock()
			l.waiting--
			l.signal()
			l.mu.Unlock()
			return ctx.Err()
		}
		l.mu.Lock()
	}
	l.waiting--
	l.writer = true
	l.mu.Unlock()
	return nil
}

// Unlock releases the exclusive lock.
func (l *rwLock) Unlock() {
	l.mu.Lock()
	l.writer = false
	l.signal()
	l.mu.Unlock()
}
