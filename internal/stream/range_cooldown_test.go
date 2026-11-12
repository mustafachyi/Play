package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"play/internal/httpretry"
)

func TestNativePlaybackDoesNotBypassSourceCooldownAfterPrefix(t *testing.T) {
	executable, err := exec.LookPath("mpv.exe")
	if err != nil {
		t.Skip("mpv.exe is not available on PATH")
	}
	payload := playbackWAV()
	var upstreamCalls, localCalls atomic.Int32
	var mutex sync.Mutex
	var requestedAt []time.Time
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)
		mutex.Lock()
		requestedAt = append(requestedAt, time.Now())
		mutex.Unlock()
		if call > 1 {
			w.Header().Set("Retry-After", "31")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		span, err := parseRange(r.Header.Get("Range"), int64(len(payload)))
		if err != nil || span.start != 0 {
			t.Errorf("native initial range = %+v, error = %v", span, err)
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", span.start, span.end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(span.end-span.start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[:32768])
	}))
	defer upstream.Close()
	gateway, err := StartDynamic(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	gateway.client.CloseIdleConnections()
	gateway.client = upstream.Client()
	if err := gateway.Add([]Resource{{Name: "audio.wav", URL: upstream.URL, MIME: "audio/wav", Size: int64(len(payload)), Ranged: true}}); err != nil {
		t.Fatal(err)
	}
	gateway.httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localCalls.Add(1)
		gateway.ServeHTTP(w, r)
	})
	position, reason := observeNativeRangeOpening(t, executable, gateway.URL("audio.wav"))
	mutex.Lock()
	defer mutex.Unlock()
	var elapsed []time.Duration
	for _, value := range requestedAt {
		elapsed = append(elapsed, value.Sub(requestedAt[0]))
	}
	t.Logf("native prefix cooldown: position = %.3fs, reason = %s, upstream = %d, local = %d, request times = %v", position, reason, upstreamCalls.Load(), localCalls.Load(), elapsed)
	if upstreamCalls.Load() != 2 || localCalls.Load() < 2 {
		t.Fatalf("native reconnect bypassed server cooldown: upstream = %d, local = %d", upstreamCalls.Load(), localCalls.Load())
	}
}

func TestSourceCooldownRejectsFreshRequestsAndExpires(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusPartialContent, ContentLength: 8, Header: http.Header{"Content-Range": {"bytes 0-7/8"}}, Body: io.NopCloser(strings.NewReader("abcdefgh"))}, nil
	})}
	handler := &sourceHandler{client: client, source: Resource{URL: "https://example.com/video", Size: 8}}
	retryAt := time.Now().Add(time.Minute)
	handler.retainCooldown(fmt.Errorf("source cooldown: %w", &httpretry.DelayTooLongError{RetryAt: retryAt}))
	written, retryable, err := handler.fetchPart(context.Background(), io.Discard, 0, 7, 8)
	if written != 0 || retryable || !errors.Is(err, httpretry.ErrDelayTooLong) || calls.Load() != 0 {
		t.Fatalf("fresh range bypassed active cooldown: written = %d, retryable = %v, calls = %d, error = %v", written, retryable, calls.Load(), err)
	}
	if err := handler.copySpan(context.Background(), io.Discard, 0, 7, 8); !errors.Is(err, httpretry.ErrDelayTooLong) || calls.Load() != 0 {
		t.Fatalf("fresh copy bypassed active cooldown: calls = %d, error = %v", calls.Load(), err)
	}
	handler.cooldownMu.Lock()
	handler.cooldownEnds = time.Now().Add(-time.Nanosecond)
	handler.cooldownMu.Unlock()
	var dst bytes.Buffer
	if err := handler.copySpan(context.Background(), &dst, 0, 7, 8); err != nil || dst.String() != "abcdefgh" || calls.Load() != 1 {
		t.Fatalf("expired cooldown did not reopen the source: body = %q, calls = %d, error = %v", dst.String(), calls.Load(), err)
	}
}

type queuedProbeContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *queuedProbeContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestSourceCooldownProtectsQueuedSizeProbes(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-release:
			}
		}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"31"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	handler := &sourceHandler{client: client, source: Resource{URL: "https://example.com/video"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 2)
	go func() { _, err := handler.contentSize(ctx); done <- err }()
	<-started
	waiterContext := &queuedProbeContext{Context: ctx, entered: make(chan struct{})}
	go func() { _, err := handler.contentSize(waiterContext); done <- err }()
	<-waiterContext.entered
	unblock()
	for range 2 {
		if err := <-done; !errors.Is(err, httpretry.ErrDelayTooLong) {
			t.Fatalf("size probe lost cooldown classification: %v", err)
		}
	}
	if _, err := handler.contentSize(ctx); !errors.Is(err, httpretry.ErrDelayTooLong) || calls.Load() != 1 {
		t.Fatalf("queued probe bypassed source cooldown: calls = %d, error = %v", calls.Load(), err)
	}
}

func TestSourceCooldownKeepsTheLongestConcurrentDeadline(t *testing.T) {
	handler := &sourceHandler{}
	now := time.Now()
	var group sync.WaitGroup
	for index := range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			handler.retainCooldown(&httpretry.DelayTooLongError{RetryAt: now.Add(time.Duration(index+1) * time.Hour)})
		}()
	}
	group.Wait()
	var delayed *httpretry.DelayTooLongError
	if err := handler.checkCooldown(context.Background()); !errors.As(err, &delayed) || !delayed.RetryAt.Equal(now.Add(32*time.Hour)) {
		t.Fatalf("concurrent replies shortened cooldown: %v", err)
	}
	handler.retainCooldown(&httpretry.DelayTooLongError{})
	handler.retainCooldown(&httpretry.DelayTooLongError{RetryAt: now.Add(48 * time.Hour)})
	if err := handler.checkCooldown(context.Background()); !errors.As(err, &delayed) || !delayed.RetryAt.IsZero() {
		t.Fatalf("finite reply weakened an unrepresentable source cooldown: %v", err)
	}
}

func TestSourceCooldownAllowsCancellationWithoutHoldingNetworkLock(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	handler := &sourceHandler{client: client, source: Resource{URL: "https://example.com/video", Size: 8}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := handler.fetchPart(ctx, io.Discard, 0, 7, 8); done <- err }()
	<-started
	stored := make(chan struct{})
	go func() {
		handler.retainCooldown(&httpretry.DelayTooLongError{RetryAt: time.Now().Add(time.Minute)})
		close(stored)
	}()
	select {
	case <-stored:
	case <-time.After(time.Second):
		cancel()
		<-done
		<-stored
		t.Fatal("source cooldown held its lock during upstream I/O")
	}
	var group sync.WaitGroup
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, retryable, err := handler.fetchPart(context.Background(), io.Discard, 0, 7, 8)
			if retryable || !errors.Is(err, httpretry.ErrDelayTooLong) {
				t.Errorf("concurrent range bypassed cooldown: retryable = %v, error = %v", retryable, err)
			}
		}()
	}
	group.Wait()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight request error = %v; want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight request ignored downstream cancellation")
	}
	_, retryable, err := handler.fetchPart(ctx, io.Discard, 0, 7, 8)
	if retryable || !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("cancellation or cooldown admitted new requests: retryable = %v, calls = %d, error = %v", retryable, calls.Load(), err)
	}
}
