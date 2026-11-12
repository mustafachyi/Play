package stream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"play/internal/httpretry"
)

type upstreamStatusError struct {
	operation  string
	status     int
	retryAfter string
	receivedAt time.Time
}

type rangeRetryExhaustedError struct {
	attempts      int
	progressBytes int64
	cause         error
}

func (e *rangeRetryExhaustedError) Error() string {
	return fmt.Sprintf("upstream range request failed after %d attempts without %d bytes of progress: %v", e.attempts, e.progressBytes, e.cause)
}

func (e *rangeRetryExhaustedError) Unwrap() error {
	return e.cause
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d", e.operation, e.status)
}

func newStatusError(operation string, response *http.Response) error {
	return &upstreamStatusError{operation: operation, status: response.StatusCode, retryAfter: response.Header.Get("Retry-After"), receivedAt: time.Now()}
}

func waitRetry(ctx context.Context, attempt int, lastErr error) error {
	var upstream *upstreamStatusError
	if errors.As(lastErr, &upstream) {
		err := httpretry.Wait(ctx, attempt, upstream.retryAfter, upstream.receivedAt)
		if errors.Is(err, httpretry.ErrDelayTooLong) {
			return errors.Join(lastErr, err)
		}
		return err
	}
	return httpretry.Wait(ctx, attempt, "", time.Time{})
}

func (h *sourceHandler) checkCooldown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.cooldownMu.Lock()
	defer h.cooldownMu.Unlock()
	if h.cooldownErr != nil && !h.cooldownEnds.IsZero() && !time.Now().Before(h.cooldownEnds) {
		h.cooldownErr = nil
		h.cooldownEnds = time.Time{}
	}
	return h.cooldownErr
}

func (h *sourceHandler) retainCooldown(err error) {
	var delayed *httpretry.DelayTooLongError
	if !errors.As(err, &delayed) {
		return
	}
	h.cooldownMu.Lock()
	defer h.cooldownMu.Unlock()
	if h.cooldownErr == nil || !h.cooldownEnds.IsZero() && (delayed.RetryAt.IsZero() || delayed.RetryAt.After(h.cooldownEnds)) {
		h.cooldownErr = err
		h.cooldownEnds = delayed.RetryAt
	}
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status == http.StatusInternalServerError || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}
