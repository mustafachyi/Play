package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"play/internal/httpretry"
)

func TestAssetRejectionDoesNotWaitForResponseBody(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "8")
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer upstream.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				body, contentType, retryable, err := fetchAssetOnce(ctx, upstream.Client(), upstream.URL)
				if body != nil || contentType != "" || retryable != retryableStatus(status) || err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
					t.Errorf("body = %q, content type = %q, retryable = %v, error = %v", body, contentType, retryable, err)
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("known asset error waited for a stalled response body")
			}
		})
	}
}

func TestAssetRecoveryHonorsServerCooldown(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			var readyAt atomic.Int64
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					readyAt.Store(time.Now().Add(time.Second).UnixNano())
				}
				if time.Now().UnixNano() < readyAt.Load() {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte("asset"))
			}))
			defer upstream.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			body, _, err := fetchAsset(ctx, upstream.Client(), upstream.URL)
			if err != nil || string(body) != "asset" || calls.Load() != 2 {
				t.Fatalf("calls = %d, body = %q, error = %v", calls.Load(), body, err)
			}
		})
	}
}

func TestAssetRecoveryStopsForExcessiveCooldown(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"31"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	_, _, err := fetchAsset(context.Background(), client, "https://example.com/asset")
	var upstream *upstreamStatusError
	if calls.Load() != 1 || !errors.Is(err, httpretry.ErrDelayTooLong) || !errors.As(err, &upstream) || upstream.status != http.StatusServiceUnavailable {
		t.Fatalf("calls = %d, error = %v", calls.Load(), err)
	}
}

func TestAssetRecoveryCancelsServerCooldown(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"1"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := fetchAsset(ctx, client, "https://example.com/asset")
	if calls.Load() != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("calls = %d, error = %v", calls.Load(), err)
	}
}
