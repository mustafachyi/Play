package httpretry

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestRemainingDelay(t *testing.T) {
	receivedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		value   string
		elapsed time.Duration
		want    time.Duration
	}{
		{name: "seconds", value: "10", elapsed: 2 * time.Second, want: 8 * time.Second},
		{name: "seconds boundary", value: "30", want: 30 * time.Second},
		{name: "leading zeros", value: "00010", elapsed: time.Second, want: 9 * time.Second},
		{name: "whitespace", value: " 2 ", want: 2 * time.Second},
		{name: "zero", value: "0"},
		{name: "expired seconds", value: "1", elapsed: 2 * time.Second},
		{name: "long hint within remaining budget", value: "60", elapsed: 40 * time.Second, want: 20 * time.Second},
		{name: "long hint elapsed", value: "60", elapsed: 61 * time.Second},
		{name: "date", value: receivedAt.Add(10 * time.Second).Format(http.TimeFormat), elapsed: 2 * time.Second, want: 8 * time.Second},
		{name: "date boundary", value: receivedAt.Add(30 * time.Second).Format(http.TimeFormat), want: 30 * time.Second},
		{name: "past date", value: receivedAt.Add(-time.Second).Format(http.TimeFormat)},
		{name: "expired date", value: receivedAt.Add(time.Second).Format(http.TimeFormat), elapsed: 2 * time.Second},
		{name: "long date within remaining budget", value: receivedAt.Add(time.Minute).Format(http.TimeFormat), elapsed: 40 * time.Second, want: 20 * time.Second},
		{name: "empty"},
		{name: "invalid", value: "later"},
		{name: "negative", value: "-1"},
		{name: "signed", value: "+1"},
		{name: "fractional", value: "0.5"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := remainingDelay(test.value, receivedAt, receivedAt.Add(test.elapsed))
			if err != nil || got != test.want {
				t.Fatalf("delay = %v, error = %v; want delay = %v, error = nil", got, err, test.want)
			}
		})
	}
}

func TestRemainingDelayRejectsExcessiveCooldowns(t *testing.T) {
	receivedAt := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value   string
		retryAt time.Time
	}{{value: "31", retryAt: receivedAt.Add(31 * time.Second)}, {value: "99999999999999999999999999999999999999999999999999"}, {value: receivedAt.Add(31 * time.Second).Format(http.TimeFormat), retryAt: receivedAt.Add(31 * time.Second)}} {
		_, err := remainingDelay(test.value, receivedAt, receivedAt)
		var delayed *DelayTooLongError
		if !errors.Is(err, ErrDelayTooLong) || !errors.As(err, &delayed) || !delayed.RetryAt.Equal(test.retryAt) || err.Error() != ErrDelayTooLong.Error() {
			t.Fatalf("value = %q, error = %v, delay error = %v; want retry deadline %v", test.value, err, delayed, test.retryAt)
		}
	}
}

func TestRemainingDelayPreservesMonotonicDeadline(t *testing.T) {
	receivedAt := time.Now()
	_, err := remainingDelay("31", receivedAt, receivedAt)
	var delayed *DelayTooLongError
	if !errors.As(err, &delayed) || delayed.RetryAt.Sub(receivedAt) != 31*time.Second || delayed.RetryAt == delayed.RetryAt.Round(0) {
		t.Fatalf("relative cooldown lost its monotonic deadline: %v", err)
	}
}

func TestWaitPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wait(ctx, 1, "31", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := Wait(ctx, 1, "30", time.Now()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestWaitUsesReceiptRelativeCooldown(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if err := Wait(ctx, 1, "1", started.Add(-800*time.Millisecond)); err != nil {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("waited %v before the cooldown elapsed", elapsed)
	}
}

func TestWaitAcceptsOriginallyLongElapsedHint(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if err := Wait(ctx, 1, "60", started.Add(-60*time.Second+50*time.Millisecond)); err != nil {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("waited %v before the base backoff elapsed", elapsed)
	}
}

func TestWaitRetainsExistingBackoff(t *testing.T) {
	for _, attempt := range []int{1, 2} {
		started := time.Now()
		if err := Wait(context.Background(), attempt, "invalid", started); err != nil {
			t.Fatal(err)
		}
		minimum := 150 * time.Millisecond
		if attempt > 1 {
			minimum = 400 * time.Millisecond
		}
		if elapsed := time.Since(started); elapsed < minimum {
			t.Fatalf("attempt %d waited %v; want at least %v", attempt, elapsed, minimum)
		}
	}
}

func TestWaitRejectsLongCooldownWithoutWaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Wait(ctx, 1, "31", time.Now()); !errors.Is(err, ErrDelayTooLong) {
		t.Fatalf("error = %v", err)
	}
}
