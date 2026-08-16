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
)

func TestParseRange(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		size    int64
		want    byteRange
		wantErr bool
	}{
		{"full", "", 100, byteRange{start: 0, end: 99}, false},
		{"bounded", "bytes=10-19", 100, byteRange{start: 10, end: 19, partial: true}, false},
		{"open", "bytes=90-", 100, byteRange{start: 90, end: 99, partial: true}, false},
		{"suffix", "bytes=-10", 100, byteRange{start: 90, end: 99, partial: true}, false},
		{"suffix larger", "bytes=-200", 100, byteRange{start: 0, end: 99, partial: true}, false},
		{"clamped", "bytes=90-200", 100, byteRange{start: 90, end: 99, partial: true}, false},
		{"multiple", "bytes=0-1,4-5", 100, byteRange{}, true},
		{"past end", "bytes=100-", 100, byteRange{}, true},
		{"backwards", "bytes=20-10", 100, byteRange{}, true},
		{"zero suffix", "bytes=-0", 100, byteRange{}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRange(test.value, test.size)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("parseRange(%q, %d) = %#v, %v; want %#v, err=%v", test.value, test.size, got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestDynamicServerAddsResourcesAtomically(t *testing.T) {
	server, err := StartDynamic(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	resources := []Resource{
		{Name: "v01.webm", URL: "https://example.com/video", Ranged: true, Size: 100},
		{Name: "a01.m4a", URL: "https://example.com/audio", Ranged: true, Size: 50},
	}
	if err := server.Add(resources); err != nil {
		t.Fatal(err)
	}
	if server.URL("v01.webm") == "" || server.URL("a01.m4a") == "" {
		t.Fatal("added resource URL missing")
	}
	if err := server.Add([]Resource{{Name: "v01.webm", URL: "https://example.com/other"}, {Name: "new", URL: "https://example.com/new"}}); err == nil {
		t.Fatal("expected duplicate rejection")
	}
	if server.URL("new") != "" {
		t.Fatal("failed batch was partially added")
	}
}

func TestDynamicServerRejectsAddAfterClose(t *testing.T) {
	server, err := StartDynamic(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Add([]Resource{{Name: "x", URL: "https://example.com/x"}}); err == nil {
		t.Fatal("expected closed server error")
	}
}

func TestSourceHandlerUsesKnownSizeWithoutProbe(t *testing.T) {
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected upstream request")
	})}
	handler := &sourceHandler{client: client, source: Resource{Name: "v01.webm", URL: "https://example.com/video", MIME: "video/webm", Ranged: true, Size: 12345}}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "http://local/v01.webm", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Length") != "12345" || requests.Load() != 0 {
		t.Fatalf("status=%d length=%q requests=%d", recorder.Code, recorder.Header().Get("Content-Length"), requests.Load())
	}
}

func TestSourceHandlerProbesSizeLazilyAndCachesIt(t *testing.T) {
	payload := []byte("0123456789ABCDEFGHIJ")
	var probes atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" {
			t.Errorf("unexpected range = %q", r.Header.Get("Range"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		probes.Add(1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(payload)))
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[:1])
	}))
	defer upstream.Close()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{Name: "v01.webm", URL: upstream.URL, Ranged: true}}
	for range 2 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "http://local/v01.webm", nil))
		if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Length") != fmt.Sprint(len(payload)) {
			t.Fatalf("response = %d length=%q", recorder.Code, recorder.Header().Get("Content-Length"))
		}
	}
	if probes.Load() != 1 {
		t.Fatalf("size probes = %d; want 1", probes.Load())
	}
}

func TestSourceHandlerSplitsUpstreamRanges(t *testing.T) {
	payload := []byte("0123456789ABCDEFGHIJ")
	var mu sync.Mutex
	var ranges []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "bytes=0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(payload)))
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[:1])
			return
		}
		mu.Lock()
		ranges = append(ranges, rangeHeader)
		mu.Unlock()
		var start, end int
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer upstream.Close()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{Name: "v01.webm", URL: upstream.URL, MIME: "video/webm", Ranged: true}, chunkSize: 4}
	local := httptest.NewServer(handler)
	defer local.Close()
	req, err := http.NewRequest(http.MethodGet, local.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=2-10")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent || string(body) != string(payload[2:11]) {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	mu.Lock()
	gotRanges := append([]string(nil), ranges...)
	mu.Unlock()
	wantRanges := []string{"bytes=2-5", "bytes=6-9", "bytes=10-10"}
	if !reflect.DeepEqual(gotRanges, wantRanges) {
		t.Fatalf("ranges = %#v; want %#v", gotRanges, wantRanges)
	}
}

func TestSourceHandlerRetriesIncompleteChunkWithoutDuplicatingBytes(t *testing.T) {
	payload := []byte("abcdefgh")
	var calls int
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Range", "bytes 0-7/8")
			w.Header().Set("Content-Length", "8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[:3])
			return
		}
		if r.Header.Get("Range") != "bytes=3-7" {
			t.Errorf("retry range = %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", "bytes 3-7/8")
		w.Header().Set("Content-Length", "5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[3:])
	}))
	defer upstream.Close()
	var dst bytes.Buffer
	handler := &sourceHandler{client: upstream.Client(), source: Resource{Name: "v01.webm", URL: upstream.URL, Ranged: true}, chunkSize: 8}
	if err := handler.copySpan(context.Background(), &dst, 0, 7, 8); err != nil {
		t.Fatal(err)
	}
	if dst.String() != string(payload) || calls != 2 {
		t.Fatalf("body=%q calls=%d", dst.String(), calls)
	}
}

func TestSourceHandlerAcceptsShorterUpstreamRanges(t *testing.T) {
	payload := []byte("abcdefgh")
	var ranges []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if end-start > 2 {
			end = start + 2
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer upstream.Close()
	var dst bytes.Buffer
	handler := &sourceHandler{client: upstream.Client(), source: Resource{Name: "v01.webm", URL: upstream.URL, Ranged: true}, chunkSize: 8}
	if err := handler.copySpan(context.Background(), &dst, 0, 7, 8); err != nil {
		t.Fatal(err)
	}
	if dst.String() != string(payload) || !reflect.DeepEqual(ranges, []string{"bytes=0-7", "bytes=3-7", "bytes=6-7"}) {
		t.Fatalf("body=%q ranges=%#v", dst.String(), ranges)
	}
}

func TestSourceHandlerReportsUpstreamFailure(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer upstream.Close()
	var reported string
	handler := &sourceHandler{client: upstream.Client(), source: Resource{Name: "v04.mp4", URL: upstream.URL, Ranged: true}, reporter: func(name string, err error) { reported = name + ": " + err.Error() }}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "http://local/v04.mp4", nil))
	if recorder.Code != http.StatusBadGateway || !strings.Contains(reported, "v04.mp4") || !strings.Contains(reported, "HTTP 403") {
		t.Fatalf("status=%d reported=%q", recorder.Code, reported)
	}
}

func TestProbeSizeConsumesSuccessfulResponseBody(t *testing.T) {
	body := strings.NewReader("x")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			ContentLength: 1,
			Header:        http.Header{"Content-Range": []string{"bytes 0-0/12345"}},
			Body:          io.NopCloser(body),
		}, nil
	})}
	size, retryable, err := probeSizeOnce(context.Background(), client, "https://example.com/video")
	if err != nil || retryable || size != 12345 || body.Len() != 0 {
		t.Fatalf("size=%d retryable=%v remaining=%d err=%v", size, retryable, body.Len(), err)
	}
}

func TestProbeSizeRetriesTransientStatus(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-0/12345")
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{'x'})
	}))
	defer upstream.Close()
	size, err := probeSize(context.Background(), upstream.Client(), upstream.URL)
	if err != nil || size != 12345 || calls.Load() != 2 {
		t.Fatalf("size=%d calls=%d err=%v", size, calls.Load(), err)
	}
}

func TestAssetHandlerProxiesBodyAndMIME(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("image"))
	}))
	defer upstream.Close()
	handler := &assetHandler{client: upstream.Client(), source: Resource{Name: "cover.jpg", URL: upstream.URL}}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://local/cover.jpg", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "image" || recorder.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("response = %d %q %q", recorder.Code, recorder.Body.String(), recorder.Header().Get("Content-Type"))
	}
}

func TestEncodedUpstreamResponsesAreRejected(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte("encoded"))
	}))
	defer upstream.Close()
	handler := &assetHandler{client: upstream.Client(), source: Resource{Name: "s01.vtt", URL: upstream.URL}}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://local/s01.vtt", nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("asset status = %d", recorder.Code)
	}
	resp := &http.Response{StatusCode: http.StatusPartialContent, ContentLength: 4, Header: http.Header{"Content-Range": []string{"bytes 0-3/4"}, "Content-Encoding": []string{"gzip"}}}
	if _, err := validateRangeResponse(resp, 0, 3, 4); err == nil {
		t.Fatal("expected encoded media response to be rejected")
	}
}

func TestValidateResource(t *testing.T) {
	for _, resource := range []Resource{
		{Name: "v01.webm", URL: "https://example.com/video", Ranged: true, Size: 123},
		{Name: "a01.m4a", URL: "https://example.com/audio", Ranged: true},
		{Name: "s01.vtt", URL: "https://example.com/sub"},
	} {
		if err := validateResource(resource); err != nil {
			t.Fatalf("validateResource(%#v): %v", resource, err)
		}
	}
	for _, resource := range []Resource{
		{Name: "../x", URL: "https://example.com/x"},
		{Name: "x", URL: "http://example.com/x"},
		{Name: "x", URL: "https://user@example.com/x"},
		{Name: "x", URL: "https://example.com/x", Size: -1},
	} {
		if err := validateResource(resource); err == nil {
			t.Fatalf("validateResource(%#v) unexpectedly succeeded", resource)
		}
	}
}

func TestMaxChunkSizeBelowTenMillionBytes(t *testing.T) {
	if maxChunkSize >= 10_000_000 || maxChunkSize < 9_500_000 {
		t.Fatalf("maxChunkSize = %d", maxChunkSize)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestStartedServerRoutesOnlyRegisteredTokenizedResources(t *testing.T) {
	server, err := Start([]Resource{{Name: "v01.webm", URL: "https://example.com/video", MIME: "video/webm", Ranged: true, Size: 123}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	url := server.URL("v01.webm")
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Length") != "123" {
		t.Fatalf("status=%d length=%q", resp.StatusCode, resp.Header.Get("Content-Length"))
	}

	badMethod, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(badMethod)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", resp.StatusCode)
	}

	wrong := strings.Replace(url, "/v01.webm", "/missing", 1)
	resp, err = http.Get(wrong)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status = %d", resp.StatusCode)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if server.URL("v01.webm") != "" {
		t.Fatal("closed server still returns resource URL")
	}
}
