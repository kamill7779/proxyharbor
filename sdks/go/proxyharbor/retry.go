package proxyharbor

import (
	"context"
	"math/rand"
	"time"
)

// backoff returns the wait duration for a given retry attempt (1-indexed).
func backoff(attempt int, cfg RetryConfig) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := cfg.MinBackoff << (attempt - 1)
	if d <= 0 || d > cfg.MaxBackoff {
		d = cfg.MaxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(cfg.MinBackoff)))
	return d + jitter
}

// sleepCtx sleeps for d while honouring context cancellation.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
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
