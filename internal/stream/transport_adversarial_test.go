package stream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWholeAssetRejectsUnsolicitedPartialContent(t *testing.T) {
	for _, contentRange := range []string{"bytes 4-7/8", "", "bytes 0-7/8"} {
		t.Run(contentRange, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "" {
					t.Errorf("whole asset requested range %q", r.Header.Get("Range"))
				}
				if contentRange != "" {
					w.Header().Set("Content-Range", contentRange)
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "half")
			}))
			defer upstream.Close()
			body, contentType, retryable, err := fetchAssetOnce(context.Background(), upstream.Client(), upstream.URL)
			if err == nil || body != nil || contentType != "" || retryable {
				t.Fatalf("body = %q, content type = %q, retryable = %v, error = %v", body, contentType, retryable, err)
			}
		})
	}
}

func TestWholeAssetPartialResponseUsesCompleteFallback(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/partial" {
			w.Header().Set("Content-Range", "bytes 4-7/8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "half")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = io.WriteString(w, "complete-image")
	}))
	defer upstream.Close()
	handler := &assetHandler{client: upstream.Client(), source: Resource{
		Name: "cover.jpg", URL: upstream.URL + "/partial", FallbackURLs: []string{upstream.URL + "/complete"},
	}}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://local/cover.jpg", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "complete-image" || recorder.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("status = %d, body = %q, content type = %q", recorder.Code, recorder.Body.String(), recorder.Header().Get("Content-Type"))
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(requests, []string{"/partial", "/complete"}) {
		t.Fatalf("requests = %v", requests)
	}
}

func TestWholeAssetPartialRejectionDoesNotReadStalledBody(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 4-7/8")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := fetchAssetOnce(ctx, upstream.Client(), upstream.URL)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), fmt.Sprint(http.StatusPartialContent)) {
			t.Fatalf("partial response error = %v", err)
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("partial asset rejection waited for the response body")
	}
}

func TestServerCloseCancelsActiveUpstreamTransfers(t *testing.T) {
	for _, ranged := range []bool{false, true} {
		t.Run(fmt.Sprint(ranged), func(t *testing.T) {
			started, canceled := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "8")
				if ranged {
					w.Header().Set("Content-Range", "bytes 0-7/8")
					w.WriteHeader(http.StatusPartialContent)
				}
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
				close(canceled)
			}))
			defer upstream.Close()
			gateway, err := StartDynamic(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close()
			gateway.client.CloseIdleConnections()
			gateway.client = upstream.Client()
			if err := gateway.Add([]Resource{{Name: "transfer", URL: upstream.URL, Size: 8, Ranged: ranged}}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL("transfer"), nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				response, err := http.DefaultClient.Do(request)
				if err == nil {
					_, err = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
				}
				done <- err
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("local transfer did not reach upstream")
			}
			if err := gateway.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("server closure left the upstream transfer active")
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("closed transfer incorrectly completed")
				}
			case <-time.After(time.Second):
				t.Fatal("server closure left the local transfer active")
			}
		})
	}
}

func TestWholeAssetEnforcesDeclaredAndStreamingLimits(t *testing.T) {
	for _, test := range []struct {
		name       string
		declared   int64
		available  int64
		wantRead   int64
		wantLength int
		wantError  bool
	}{
		{name: "declared oversized", declared: maxAssetSize + 1, available: maxAssetSize + 2, wantError: true},
		{name: "streamed oversized", declared: -1, available: maxAssetSize + 2, wantRead: maxAssetSize + 1, wantError: true},
		{name: "exact limit", declared: -1, available: maxAssetSize, wantRead: maxAssetSize, wantLength: int(maxAssetSize)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var read int64
			reader := progressReadFunc(func(p []byte) (int, error) {
				clear(p)
				read += int64(len(p))
				return len(p), nil
			})
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: test.declared,
					Body: io.NopCloser(io.LimitReader(reader, test.available)),
				}, nil
			})}
			body, _, retryable, err := fetchAssetOnce(context.Background(), client, "https://example.com/asset")
			if (err != nil) != test.wantError || retryable || read != test.wantRead || len(body) != test.wantLength {
				t.Fatalf("read = %d, body length = %d, retryable = %v, error = %v; want read = %d, length = %d, error = %v", read, len(body), retryable, err, test.wantRead, test.wantLength, test.wantError)
			}
		})
	}
}
