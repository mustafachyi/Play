package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWholeAssetHTTP2ConcurrentStreamsAndIndependentCancellation(t *testing.T) {
	const concurrency = 16
	const assetSize = 256 << 10
	payload := make([]byte, assetSize)
	for index := range payload {
		payload[index] = byte(index % 251)
	}
	started := make(chan int, concurrency)
	canceled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var connections, protocolErrors atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			protocolErrors.Add(1)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.URL.Path == "/warm" {
			_, _ = w.Write([]byte("ready"))
			return
		}
		index, err := strconv.Atoi(r.URL.Query().Get("id"))
		if err != nil || index < 0 || index >= concurrency {
			http.Error(w, "invalid asset", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(assetSize))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		started <- index
		if index == 0 {
			<-r.Context().Done()
			close(canceled)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		if _, err := w.Write([]byte{byte(index)}); err == nil {
			_, _ = w.Write(payload[1:])
		}
	}))
	upstream.EnableHTTP2 = true
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	upstream.StartTLS()
	client := upstream.Client()
	transport := client.Transport.(*http.Transport)
	transport.MaxConnsPerHost = 1
	transport.MaxIdleConnsPerHost = 1
	transport.DisableCompression = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() {
		cancel()
		unblock()
		client.CloseIdleConnections()
		upstream.Close()
	})
	body, contentType, _, err := fetchAssetOnce(ctx, client, upstream.URL+"/warm")
	if err != nil || string(body) != "ready" || contentType != "application/octet-stream" {
		t.Fatalf("warm body = %q, content type = %q, error = %v", body, contentType, err)
	}
	type result struct {
		index int
		bytes int
		valid bool
		err   error
	}
	results := make(chan result, concurrency)
	streamContext, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	for index := range concurrency {
		go func(index int) {
			requestContext := ctx
			if index == 0 {
				requestContext = streamContext
			}
			body, contentType, _, err := fetchAssetOnce(requestContext, client, fmt.Sprintf("%s/asset?id=%d", upstream.URL, index))
			valid := len(body) == assetSize && body[0] == byte(index) && bytes.Equal(body[1:], payload[1:]) && contentType == "application/octet-stream"
			results <- result{index: index, bytes: len(body), valid: valid, err: err}
		}(index)
	}
	seen := make(map[int]bool, concurrency)
	for range concurrency {
		select {
		case index := <-started:
			if seen[index] {
				t.Fatalf("duplicate active stream %d", index)
			}
			seen[index] = true
		case <-ctx.Done():
			t.Fatalf("only %d/%d streams became active on the warmed connection: %v", len(seen), concurrency, ctx.Err())
		}
	}
	cancelStream()
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("canceling one stream did not reach its upstream handler")
	}
	select {
	case result := <-results:
		if result.index != 0 || result.bytes != 0 || !errors.Is(result.err, context.Canceled) {
			t.Fatalf("first completed stream = %+v; want only the canceled stream", result)
		}
	case <-ctx.Done():
		t.Fatal("canceled asset fetch did not return")
	}
	unblock()
	delivered := 0
	for range concurrency - 1 {
		select {
		case result := <-results:
			if result.index == 0 || result.err != nil || !result.valid {
				t.Fatalf("healthy stream failed after independent cancellation: %+v", result)
			}
			delivered += result.bytes
		case <-ctx.Done():
			t.Fatal("healthy asset streams did not finish")
		}
	}
	if delivered != (concurrency-1)*assetSize || connections.Load() != 1 || protocolErrors.Load() != 0 {
		t.Fatalf("delivered = %d, connections = %d, protocol mismatches = %d", delivered, connections.Load(), protocolErrors.Load())
	}
	t.Logf("HTTP/2: %d simultaneous streams on %d warmed connection, one canceled independently, %d exact payload bytes delivered", concurrency, connections.Load(), delivered)
}
