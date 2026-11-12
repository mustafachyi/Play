package youtube

import (
	"context"
	"errors"

	"play/internal/httpretry"
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
		upstream := transportError(err)
		if waitErr := httpretry.Wait(ctx, attempt+1, upstream.RetryAfter, upstream.ReceivedAt); waitErr != nil {
			if errors.Is(waitErr, httpretry.ErrDelayTooLong) {
				return nil, errors.Join(err, waitErr)
			}
			return nil, waitErr
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
