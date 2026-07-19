package engine

import (
	"context"
	"sync"
	"time"
)

// contextMutex is a zero-value mutex whose wait can be interrupted by a
// request context. The semantic resident barrier can be held for the duration
// of a bounded Flat scan, so a plain sync.Mutex would make a cancelled MCP
// request wait for an unrelated query to finish.
type contextMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *contextMutex) init() {
	m.once.Do(func() {
		m.token = make(chan struct{}, 1)
		m.token <- struct{}{}
	})
}

func (m *contextMutex) Lock() {
	_ = m.LockContext(context.Background())
}

func (m *contextMutex) LockContext(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.init()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		// If cancellation raced a ready token, give cancellation precedence and
		// put the token back. This makes deterministic cancellation tests stable.
		if err := ctx.Err(); err != nil {
			m.token <- struct{}{}
			return err
		}
		return nil
	}
}

func (m *contextMutex) Unlock() {
	m.init()
	select {
	case m.token <- struct{}{}:
	default:
		panic("engine: unlock of unlocked context mutex")
	}
}

// contextRWMutex preserves the familiar sync.RWMutex API while adding
// cancellable acquisition for request paths. Existing write paths continue to
// use Lock/RLock; MCP status and semantic operations use the Context variants.
type contextRWMutex struct {
	mu sync.RWMutex
}

func (m *contextRWMutex) Lock()    { m.mu.Lock() }
func (m *contextRWMutex) Unlock()  { m.mu.Unlock() }
func (m *contextRWMutex) RLock()   { m.mu.RLock() }
func (m *contextRWMutex) RUnlock() { m.mu.RUnlock() }

func (m *contextRWMutex) LockContext(ctx context.Context) error {
	return pollContextLock(ctx, m.mu.TryLock, m.mu.Unlock)
}

func (m *contextRWMutex) RLockContext(ctx context.Context) error {
	return pollContextLock(ctx, m.mu.TryRLock, m.mu.RUnlock)
}

func pollContextLock(ctx context.Context, try func() bool, undo func()) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if try() {
		if err := ctx.Err(); err != nil {
			undo()
			return err
		}
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if !try() {
				continue
			}
			if err := ctx.Err(); err != nil {
				undo()
				return err
			}
			return nil
		}
	}
}

func contextCheckpoint(ctx context.Context, i int) error {
	if i&63 != 0 {
		return nil
	}
	return ctx.Err()
}
