package httpretry

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestRemainingDelayNanosecondBoundaries(t *testing.T) {
	receivedAt := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for seconds := int64(0); seconds <= 120; seconds++ {
		for _, elapsed := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond, 30*time.Second - time.Nanosecond, 30 * time.Second, 30*time.Second + time.Nanosecond, 121 * time.Second} {
			want := time.Duration(seconds)*time.Second - elapsed
			got, err := remainingDelay(strconv.FormatInt(seconds, 10), receivedAt, receivedAt.Add(elapsed))
			if want > 30*time.Second {
				var delayed *DelayTooLongError
				if !errors.Is(err, ErrDelayTooLong) || !errors.As(err, &delayed) || !delayed.RetryAt.Equal(receivedAt.Add(time.Duration(seconds)*time.Second)) {
					t.Fatalf("seconds=%d elapsed=%v: delay=%v error=%v", seconds, elapsed, got, err)
				}
				continue
			}
			if want < 0 {
				want = 0
			}
			if err != nil || got != want {
				t.Fatalf("seconds=%d elapsed=%v: delay=%v error=%v want=%v", seconds, elapsed, got, err, want)
			}
		}
	}
}

func TestRemainingDelayDurationOverflowBoundary(t *testing.T) {
	receivedAt := time.Now()
	maximum := int64((1<<63 - 1) / time.Second)
	for _, seconds := range []int64{maximum, maximum + 1} {
		_, err := remainingDelay(strconv.FormatInt(seconds, 10), receivedAt, receivedAt)
		var delayed *DelayTooLongError
		if !errors.As(err, &delayed) || !errors.Is(err, ErrDelayTooLong) {
			t.Fatalf("seconds=%d: error=%v", seconds, err)
		}
		if seconds == maximum && delayed.RetryAt.Sub(receivedAt) != time.Duration(seconds)*time.Second {
			t.Fatalf("representable deadline changed: %v", delayed.RetryAt)
		}
		if seconds > maximum && !delayed.RetryAt.IsZero() {
			t.Fatalf("overflow produced a fabricated deadline: %v", delayed.RetryAt)
		}
	}
}

func TestWaitConcurrentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const workers = 128
	ready := sync.WaitGroup{}
	ready.Add(workers)
	done := make(chan error, workers)
	for range workers {
		go func() {
			ready.Done()
			done <- Wait(ctx, 2, "30", time.Now())
		}()
	}
	ready.Wait()
	cancel()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for range workers {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled waiter returned %v", err)
			}
		case <-deadline.C:
			t.Fatal("canceled waiters did not terminate")
		}
	}
}

func FuzzRemainingDelay(f *testing.F) {
	for _, value := range []string{"", "0", "30", "31", "9223372036", "9223372037", "18446744073709551615", "99999999999999999999999999999999999999", "-1", "+1", "0.5", "later", "Thu, 12 Nov 2026 12:00:30 GMT", "Fri, 31 Dec 9999 23:59:59 GMT"} {
		f.Add(value, int64(0))
	}
	receivedAt := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	f.Add(receivedAt.Add(30*time.Second).Format(http.TimeFormat), int64(-1))
	f.Fuzz(func(t *testing.T, value string, elapsed int64) {
		got, err := remainingDelay(value, receivedAt, receivedAt.Add(time.Duration(elapsed)))
		if err != nil {
			var delayed *DelayTooLongError
			if got != 0 || !errors.Is(err, ErrDelayTooLong) || !errors.As(err, &delayed) {
				t.Fatalf("unexpected failure: delay=%v error=%v", got, err)
			}
			return
		}
		if got < 0 || got > 30*time.Second {
			t.Fatalf("unbounded retry wait %v for %q", got, value)
		}
	})
}
