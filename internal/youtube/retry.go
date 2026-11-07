package youtube

import (
	"context"
	"time"
)

func retryRequest(ctx context.Context, request func() ([]byte, error)) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body, err := request()
		if err == nil || attempt == 2 || !retryableError(err) {
			return body, err
		}
		delay := 150 * time.Millisecond
		if attempt > 0 {
			delay = 400 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func retryableError(err error) bool {
	upstream := transportError(err)
	if upstream == nil {
		return false
	}
	if upstream.Kind == Timeout || upstream.Kind == NetworkFailure {
		return true
	}
	if upstream.Kind != HTTPFailure {
		return false
	}
	switch upstream.Status {
	case 408, 425, 429, 500, 502, 503, 504:
		return true
	}
	return false
}
