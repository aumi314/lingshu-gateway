package probe

import (
	"context"
	"net/http"
	"sync"
	"time"
)

func isThrottleStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == 529
}

func withQualityThrottle(call Caller) Caller {
	var mu sync.Mutex
	var resumeAt time.Time
	active, limit := 0, 3
	changed := make(chan struct{})
	acquire := func(ctx context.Context) bool {
		for {
			mu.Lock()
			if ctx.Err() != nil {
				mu.Unlock()
				return false
			}
			wait := time.Until(resumeAt)
			if active < limit && wait <= 0 {
				active++
				mu.Unlock()
				return true
			}
			notify := changed
			mu.Unlock()
			var timer *time.Timer
			var ready <-chan time.Time
			if wait > 0 {
				timer = time.NewTimer(wait)
				ready = timer.C
			}
			select {
			case <-notify:
			case <-ready:
			case <-ctx.Done():
			}
			if timer != nil {
				timer.Stop()
			}
		}
	}
	release := func(result Call) {
		mu.Lock()
		if isThrottleStatus(result.HTTPStatus) {
			delay := 20 * time.Second
			if result.RetryAfter != nil {
				delay = *result.RetryAfter
			}
			if until := time.Now().Add(delay); until.After(resumeAt) {
				resumeAt = until
			}
			limit = 2
		}
		active--
		close(changed)
		changed = make(chan struct{})
		mu.Unlock()
	}
	return func(ctx context.Context, req Request) Call {
		if !acquire(ctx) {
			return Call{NotSent: true, Error: "检测已取消"}
		}
		acquired := true
		req.BeforeRetry = func() bool {
			release(Call{})
			acquired = acquire(ctx)
			return acquired
		}
		result := call(ctx, req)
		if acquired {
			release(result)
		}
		return result
	}
}
