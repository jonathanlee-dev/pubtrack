package pubmed

import (
	"context"
	"sync"
	"time"
)

// Limiter spaces requests at least 1/rate apart. NCBI blocks clients that
// exceed 3 requests/second (10 with an API key), so bursts are not allowed.
type Limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func NewLimiter(perSecond int) *Limiter {
	if perSecond <= 0 {
		perSecond = 1
	}
	return &Limiter{interval: time.Second / time.Duration(perSecond)}
}

// Wait blocks until the caller's slot comes up or ctx is done.
func (l *Limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	slot := l.next
	if now := time.Now(); slot.Before(now) {
		slot = now
	}
	l.next = slot.Add(l.interval)
	l.mu.Unlock()

	d := time.Until(slot)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
