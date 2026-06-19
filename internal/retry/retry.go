package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DelayMode controls how delays between attempts are applied.
type DelayMode int

const (
	// FixedDelay keeps the same delay between attempts.
	FixedDelay DelayMode = iota
	// BackOffDelay doubles the delay after each failure up to MaxDelay.
	BackOffDelay
)

// Option configures Do behavior.
type Option func(*config)

type config struct {
	attempts      int
	delay         time.Duration
	maxDelay      time.Duration
	delayType     DelayMode
	ctx           context.Context
	retryIf       func(error) bool
	onRetry       func(uint, error)
	lastErrorOnly bool
}

func defaultConfig() *config {
	return &config{
		attempts:      1,
		delay:         0,
		maxDelay:      0,
		delayType:     FixedDelay,
		lastErrorOnly: true,
	}
}

// Attempts sets how many times the operation should be attempted.
func Attempts(n uint) Option {
	return func(cfg *config) {
		cfg.attempts = int(n)
	}
}

// Delay configures the initial delay between attempts.
func Delay(d time.Duration) Option {
	return func(cfg *config) {
		cfg.delay = d
	}
}

// MaxDelay caps the exponential backoff delay.
func MaxDelay(d time.Duration) Option {
	return func(cfg *config) {
		cfg.maxDelay = d
	}
}

// DelayType configures fixed or exponential backoff delays.
func DelayType(dt DelayMode) Option {
	return func(cfg *config) {
		cfg.delayType = dt
	}
}

// Context sets a context that cancels retries when done.
func Context(ctx context.Context) Option {
	return func(cfg *config) {
		cfg.ctx = ctx
	}
}

// RetryIf provides a predicate to decide if an error is retryable.
func RetryIf(fn func(error) bool) Option {
	return func(cfg *config) {
		cfg.retryIf = fn
	}
}

// OnRetry is invoked after each failed attempt (before the next delay).
func OnRetry(fn func(uint, error)) Option {
	return func(cfg *config) {
		cfg.onRetry = fn
	}
}

// LastErrorOnly controls whether Do returns only the last error or wraps it.
func LastErrorOnly(lastOnly bool) Option {
	return func(cfg *config) {
		cfg.lastErrorOnly = lastOnly
	}
}

type unrecoverableError struct {
	err error
}

func (u unrecoverableError) Error() string {
	return u.err.Error()
}

func (u unrecoverableError) Unwrap() error {
	return u.err
}

// Unrecoverable marks an error so the retry loop stops immediately.
func Unrecoverable(err error) error {
	if err == nil {
		return nil
	}
	return unrecoverableError{err: err}
}

// Do executes fn up to Attempts times until it succeeds or returns an unrecoverable error.
func Do(fn func() error, opts ...Option) error {
	if fn == nil {
		return fmt.Errorf("retry: nil function")
	}

	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if cfg.attempts <= 0 {
		cfg.attempts = 1
	}

	var lastErr error
	delay := cfg.delay
	ctx := cfg.ctx

	for attempt := 1; attempt <= cfg.attempts; attempt++ {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}

		err := fn()
		if err == nil {
			return nil
		}

		unrecoverable := unrecoverableError{}
		if errors.As(err, &unrecoverable) {
			return unrecoverable.err
		}

		lastErr = err
		if cfg.retryIf != nil && !cfg.retryIf(err) {
			break
		}

		if attempt == cfg.attempts {
			break
		}

		if cfg.onRetry != nil {
			cfg.onRetry(uint(attempt), err)
		}

		if err := sleepWithContext(ctx, delay); err != nil {
			return err
		}

		if cfg.delayType == BackOffDelay && delay > 0 {
			delay *= 2
			if cfg.maxDelay > 0 && delay > cfg.maxDelay {
				delay = cfg.maxDelay
			}
		}
		if cfg.delayType == FixedDelay && cfg.maxDelay > 0 && delay > cfg.maxDelay {
			delay = cfg.maxDelay
		}
	}

	if lastErr == nil {
		return nil
	}

	if cfg.lastErrorOnly {
		return lastErr
	}

	return fmt.Errorf("retry failed after %d attempts: %w", cfg.attempts, lastErr)
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	if ctx == nil {
		<-timer.C
		return nil
	}

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
