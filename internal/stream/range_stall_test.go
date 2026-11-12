package stream

import (
	"bytes"
	"context"
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
)

func TestSourceHandlerDoesNotWaitForRejectedBody(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusForbidden)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL}}
	done := make(chan error, 1)
	go func() {
		written, retryable, err := handler.fetchPart(ctx, io.Discard, 0, 7, 8)
		if written != 0 || retryable || err == nil || !strings.Contains(err.Error(), "HTTP 403") {
			t.Errorf("written = %d, retryable = %v, error = %v", written, retryable, err)
		}
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("known HTTP error waited for a stalled response body")
	}
}

func TestSourceHandlerResumesStalledRangeAtDeliveredByte(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var ranges []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Range", "bytes 0-7/8")
			w.Header().Set("Content-Length", "8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "abc")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Range", "bytes 3-7/8")
		w.Header().Set("Content-Length", "5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "defgh")
	}))
	defer upstream.Close()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL}, readTimeout: 40 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var dst bytes.Buffer
	if err := handler.copySpan(ctx, &dst, 0, 7, 8); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if dst.String() != "abcdefgh" || !reflect.DeepEqual(ranges, []string{"bytes=0-7", "bytes=3-7"}) {
		t.Fatalf("body = %q, ranges = %v", dst.String(), ranges)
	}
}

func TestSourceHandlerBoundsRepeatedStalledRanges(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Range", "bytes 0-7/8")
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL}, readTimeout: 30 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var dst bytes.Buffer
	err := handler.copySpan(ctx, &dst, 0, 7, 8)
	if !errors.Is(err, errRangeReadStalled) || calls.Load() != maxChunkAttempts || dst.Len() != 0 {
		t.Fatalf("body = %q, calls = %d, error = %v", dst.String(), calls.Load(), err)
	}
}

func TestSourceHandlerAllowsContinuousSlowProgress(t *testing.T) {
	payload := "abcdefghijklmnop"
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		for _, value := range []byte(payload) {
			_, _ = w.Write([]byte{value})
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	defer upstream.Close()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL}, readTimeout: 50 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var dst bytes.Buffer
	if err := handler.copySpan(ctx, &dst, 0, int64(len(payload)-1), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if dst.String() != payload || calls.Load() != 1 {
		t.Fatalf("body = %q, calls = %d", dst.String(), calls.Load())
	}
}

type pausedWriter struct {
	started chan struct{}
	release chan struct{}
	body    bytes.Buffer
}

func (w *pausedWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return w.body.Write(p)
}

func TestSourceHandlerDoesNotTimeOutDownstreamBackpressure(t *testing.T) {
	requestContext := make(chan context.Context, 1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestContext <- r.Context()
		return &http.Response{StatusCode: http.StatusPartialContent, ContentLength: 8, Header: http.Header{"Content-Range": {"bytes 0-7/8"}}, Body: io.NopCloser(strings.NewReader("abcdefgh"))}, nil
	})}
	const timeout = 20 * time.Millisecond
	handler := &sourceHandler{client: client, source: Resource{URL: "https://example.com/video"}, readTimeout: timeout}
	writer := &pausedWriter{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(writer.release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() { done <- handler.copySpan(context.Background(), writer, 0, 7, 8) }()
	ctx := <-requestContext
	<-writer.started
	select {
	case <-ctx.Done():
		unblock()
		<-done
		t.Fatalf("upstream request canceled during downstream write: %v", ctx.Err())
	case <-time.After(4 * timeout):
	}
	unblock()
	if err := <-done; err != nil || writer.body.String() != "abcdefgh" {
		t.Fatalf("body = %q, error = %v", writer.body.String(), err)
	}
}

func TestSourceHandlerCancelsStalledRangeWithoutRetry(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Range", "bytes 0-7/8")
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL}}
	done := make(chan error, 1)
	go func() { done <- handler.copySpan(ctx, io.Discard, 0, 7, 8) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("calls = %d, error = %v", calls.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled media request remained blocked")
	}
}

func TestProbeSizeDoesNotWaitForRejectedBody(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		size, retryable, err := probeSizeOnce(ctx, upstream.Client(), upstream.URL)
		if size != 0 || !retryable || err == nil || !strings.Contains(err.Error(), "HTTP 503") {
			t.Errorf("size = %d, retryable = %v, error = %v", size, retryable, err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("size probe waited for a rejected response body")
	}
}

func TestSourceHandlerStallCancellationPreservesOtherHTTP2Streams(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stalled" {
			w.Header().Set("Content-Range", "bytes 0-7/8")
			w.Header().Set("Content-Length", "8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "abc")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Length", "8")
		for _, value := range []byte("abcdefgh") {
			_, _ = w.Write([]byte{value})
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	client := upstream.Client()
	handler := &sourceHandler{client: client, source: Resource{URL: upstream.URL + "/stalled"}, readTimeout: 30 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		written, retryable, err := handler.fetchPart(ctx, io.Discard, 0, 7, 8)
		if written != 3 || !retryable || !errors.Is(err, errRangeReadStalled) {
			t.Errorf("written = %d, retryable = %v, error = %v", written, retryable, err)
		}
		done <- err
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/healthy", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(body) != "abcdefgh" || response.ProtoMajor != 2 {
		t.Fatalf("body = %q, protocol = %s, error = %v", body, response.Proto, err)
	}
	<-done
}

func TestProbeSizeReturnsAfterExpectedByteWithoutWaitingForEOF(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/12345")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "x")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		size, retryable, err := probeSizeOnce(ctx, upstream.Client(), upstream.URL)
		if size != 12345 || retryable || err != nil {
			t.Errorf("size = %d, retryable = %v, error = %v", size, retryable, err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("size probe waited for bytes outside its validated one-byte range")
	}
}

func TestProbeSizeRetriesMissingExpectedByte(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/12345")
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		if calls.Add(1) > 1 {
			_, _ = io.WriteString(w, "x")
		}
	}))
	defer upstream.Close()
	size, err := probeSize(context.Background(), upstream.Client(), upstream.URL)
	if size != 12345 || err != nil || calls.Load() != 2 {
		t.Fatalf("size = %d, calls = %d, error = %v", size, calls.Load(), err)
	}
}

func TestProbeSizeCancelsStalledFirstByte(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/12345")
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), rangeReadTimeout+2*time.Second)
	defer cancel()
	size, retryable, err := probeSizeOnce(ctx, upstream.Client(), upstream.URL)
	if size != 0 || !retryable || !errors.Is(err, errRangeReadStalled) || ctx.Err() != nil {
		t.Fatalf("size = %d, retryable = %v, context error = %v, error = %v", size, retryable, ctx.Err(), err)
	}
}

type progressReadFunc func([]byte) (int, error)

func (f progressReadFunc) Read(p []byte) (int, error) {
	return f(p)
}

func TestProgressReaderIgnoresQueuedCallbackAfterProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	reads := 0
	body := progressReadFunc(func(p []byte) (int, error) {
		reads++
		if reads == 2 {
			close(started)
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-release:
			}
		}
		p[0] = 'x'
		return 1, nil
	})
	const timeout = 100 * time.Millisecond
	reader := &progressReader{reader: body, cancel: cancel, timeout: timeout}
	if _, err := reader.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * timeout)
	reader.cancelStalledRead()
	if ctx.Err() != nil {
		t.Fatal("completed read retained an active deadline")
	}
	done := make(chan error, 1)
	go func() { _, err := reader.Read(make([]byte, 1)); done <- err }()
	<-started
	reader.cancelStalledRead()
	unblock()
	if err := <-done; err != nil || ctx.Err() != nil {
		t.Fatalf("queued callback canceled the next read before its deadline: context error = %v, read error = %v", ctx.Err(), err)
	}
}
