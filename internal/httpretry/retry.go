package httpretry

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var ErrDelayTooLong = errors.New("server requested a wait longer than 30 seconds; try again later")

type DelayTooLongError struct {
	RetryAt time.Time
}

func (e *DelayTooLongError) Error() string {
	return ErrDelayTooLong.Error()
}

func (e *DelayTooLongError) Unwrap() error {
	return ErrDelayTooLong
}

func Wait(ctx context.Context, attempt int, retryAfter string, receivedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	if receivedAt.IsZero() {
		receivedAt = now
	}
	delay, err := remainingDelay(retryAfter, receivedAt, now)
	if err != nil {
		return err
	}
	backoff := 150 * time.Millisecond
	if attempt > 1 {
		backoff = 400 * time.Millisecond
	}
	if delay < backoff {
		delay = backoff
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func remainingDelay(value string, receivedAt, now time.Time) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	decimal := true
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			decimal = false
			break
		}
	}
	var retryAt time.Time
	if decimal {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64((1<<63-1)/time.Second) {
			return 0, &DelayTooLongError{}
		}
		retryAt = receivedAt.Add(time.Duration(seconds) * time.Second)
	} else {
		var err error
		retryAt, err = http.ParseTime(value)
		if err != nil {
			return 0, nil
		}
	}
	delay := retryAt.Sub(now)
	if delay > 30*time.Second {
		return 0, &DelayTooLongError{RetryAt: retryAt}
	}
	if delay > 0 {
		return delay, nil
	}
	return 0, nil
}
