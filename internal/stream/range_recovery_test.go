package stream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"play/internal/httpretry"
)

func TestSourceHandlerPreservesRecoveryAfterSubstantialProgress(t *testing.T) {
	total := maxChunkSize
	progress := maxChunkSize / 4
	var calls atomic.Int32
	var mutex sync.Mutex
	var ranges []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		mutex.Lock()
		ranges = append(ranges, rangeHeader)
		mutex.Unlock()
		var start, end int64
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil {
			t.Errorf("range = %q, error = %v", rangeHeader, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		calls.Add(1)
		length := end - start + 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", fmt.Sprint(length))
		w.WriteHeader(http.StatusPartialContent)
		if err := writeChunkPattern(w, start, min(length, progress)); err != nil {
			t.Errorf("write truncated part: %v", err)
		}
	}))
	defer upstream.Close()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL, Size: total}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	dst := &chunkDigestWriter{hash: sha256.New()}
	err := handler.copySpan(ctx, dst, 0, total-1, total)
	expectedPrefix := sha256.New()
	if prefixErr := writeChunkPattern(expectedPrefix, 0, dst.bytes); prefixErr != nil {
		t.Fatal(prefixErr)
	}
	if !bytes.Equal(dst.hash.Sum(nil), expectedPrefix.Sum(nil)) {
		t.Fatal("positive-progress recovery corrupted the forwarded prefix")
	}
	mutex.Lock()
	defer mutex.Unlock()
	wantRanges := []string{
		fmt.Sprintf("bytes=0-%d", total-1),
		fmt.Sprintf("bytes=%d-%d", progress, total-1),
		fmt.Sprintf("bytes=%d-%d", 2*progress, total-1),
		fmt.Sprintf("bytes=%d-%d", 3*progress, total-1),
	}
	if err != nil || dst.bytes != total || calls.Load() != 4 || !reflect.DeepEqual(ranges, wantRanges) {
		t.Fatalf("useful progress stopped: forwarded = %d/%d, calls = %d, ranges = %v, error = %v", dst.bytes, total, calls.Load(), ranges, err)
	}
}

func TestSourceHandlerHonorsRetryAfterBeforeRetry(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			testRangeRetryAfter(t, status)
		})
	}
}

func testRangeRetryAfter(t *testing.T, status int) {
	var calls atomic.Int32
	var retryAt time.Time
	var mutex sync.Mutex
	var elapsed []time.Duration
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		mutex.Lock()
		if call == 1 {
			retryAt = time.Now().Add(time.Second)
		}
		ready := !time.Now().Before(retryAt)
		elapsed = append(elapsed, time.Until(retryAt))
		mutex.Unlock()
		if !ready {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-7/8")
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("abcdefgh"))
	}))
	defer upstream.Close()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL, Size: 8}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var dst bytes.Buffer
	err := handler.copySpan(ctx, &dst, 0, 7, 8)
	mutex.Lock()
	defer mutex.Unlock()
	if err != nil || dst.String() != "abcdefgh" || calls.Load() != 2 {
		t.Fatalf("Retry-After was not respected: body = %q, calls = %d, remaining cooldown = %v, error = %v", dst.String(), calls.Load(), elapsed, err)
	}
}

func TestSourceHandlerProgressCreditsRemainBounded(t *testing.T) {
	tests := []struct {
		name                string
		bytesPerResponse    int64
		zerosBeforeProgress int32
		wantCalls           int32
		wantBytes           int64
		wantError           bool
	}{
		{name: "cumulative partial progress", bytesPerResponse: maxChunkSize / 8, wantCalls: 8, wantBytes: maxChunkSize},
		{name: "maximum credited attempts", bytesPerResponse: maxChunkSize / 4, zerosBeforeProgress: 2, wantCalls: 12, wantBytes: maxChunkSize},
		{name: "one byte per response exhausts retry budget", bytesPerResponse: 1, wantCalls: 3, wantBytes: 3, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var start, end int64
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
					t.Errorf("read range: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				call := calls.Add(1)
				length := end - start + 1
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, maxChunkSize))
				w.Header().Set("Content-Length", fmt.Sprint(length))
				w.WriteHeader(http.StatusPartialContent)
				if call%(test.zerosBeforeProgress+1) != 0 {
					return
				}
				if err := writeChunkPattern(w, start, min(length, test.bytesPerResponse)); err != nil {
					t.Errorf("write upstream part: %v", err)
				}
			}))
			defer upstream.Close()
			handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL, Size: maxChunkSize}}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			dst := &chunkDigestWriter{hash: sha256.New()}
			err := handler.copySpan(ctx, dst, 0, maxChunkSize-1, maxChunkSize)
			if (err != nil) != test.wantError || calls.Load() != test.wantCalls || dst.bytes != test.wantBytes {
				t.Fatalf("calls = %d, bytes = %d, error = %v; want calls = %d, bytes = %d, error present = %v", calls.Load(), dst.bytes, err, test.wantCalls, test.wantBytes, test.wantError)
			}
			if test.wantError && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("retry exhaustion lost its upstream error: %v", err)
			}
			expectedDigest := sha256.New()
			if err := writeChunkPattern(expectedDigest, 0, test.wantBytes); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(dst.hash.Sum(nil), expectedDigest.Sum(nil)) {
				t.Fatal("credited attempts duplicated or lost forwarded bytes")
			}
		})
	}
}

func TestSourceHandlerRetryAfterWaitIsCancellable(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL, Size: 8}}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := handler.copySpan(ctx, io.Discard, 0, 7, 8)
	var exhausted *rangeRetryExhaustedError
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &exhausted) || calls.Load() != 1 {
		t.Fatalf("cooldown cancellation made additional requests: calls = %d, error = %v", calls.Load(), err)
	}
}

func TestSourceHandlerRejectsExcessiveRetryAfter(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "31")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL, Size: 8}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := handler.copySpan(ctx, io.Discard, 0, 7, 8)
	var upstreamErr *upstreamStatusError
	var exhausted *rangeRetryExhaustedError
	if !errors.Is(err, httpretry.ErrDelayTooLong) || errors.As(err, &exhausted) || !errors.As(err, &upstreamErr) || upstreamErr.status != http.StatusServiceUnavailable || upstreamErr.operation != "upstream range request" || calls.Load() != 1 {
		t.Fatalf("excessive cooldown classification or request count is incorrect: calls = %d, error = %v", calls.Load(), err)
	}
}

func TestSourceHandlerChecksFinalRetryAfterBeforeExhaustion(t *testing.T) {
	for _, header := range []string{"1", "31"} {
		t.Run(header, func(t *testing.T) {
			var calls atomic.Int32
			var finalReceived time.Time
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
				if calls.Add(1) == maxChunkAttempts {
					finalReceived = time.Now()
					response.Header.Set("Retry-After", header)
				}
				return response, nil
			})}
			handler := &sourceHandler{client: client, source: Resource{URL: "https://example.com/video", Size: 8}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := handler.copySpan(ctx, io.Discard, 0, 7, 8)
			var exhausted *rangeRetryExhaustedError
			var upstream *upstreamStatusError
			if !errors.As(err, &upstream) || upstream.status != http.StatusServiceUnavailable || calls.Load() != maxChunkAttempts {
				t.Fatalf("final status context lost: calls = %d, error = %v", calls.Load(), err)
			}
			if header == "1" {
				if !errors.As(err, &exhausted) || exhausted.attempts != maxChunkAttempts || exhausted.progressBytes != 2 || time.Since(finalReceived) < time.Second {
					t.Fatalf("final cooldown bypassed before native handoff: elapsed = %v, error = %v", time.Since(finalReceived), err)
				}
			} else if !errors.Is(err, httpretry.ErrDelayTooLong) || errors.As(err, &exhausted) {
				t.Fatalf("final excessive cooldown enabled native handoff: %v", err)
			}
		})
	}
}

type rejectedRangeWriter struct{}

func (rejectedRangeWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestSourceHandlerDoesNotClassifyDownstreamFailureAsRetryExhaustion(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusPartialContent, ContentLength: 8, Header: http.Header{"Content-Range": {"bytes 0-7/8"}}, Body: io.NopCloser(strings.NewReader("abcdefgh"))}, nil
	})}
	handler := &sourceHandler{client: client, source: Resource{URL: "https://example.com/video", Size: 8}}
	err := handler.copySpan(context.Background(), rejectedRangeWriter{}, 0, 7, 8)
	var exhausted *rangeRetryExhaustedError
	if !errors.Is(err, io.ErrClosedPipe) || errors.As(err, &exhausted) || calls.Load() != 1 {
		t.Fatalf("downstream failure enabled native recovery: calls = %d, error = %v", calls.Load(), err)
	}
}
