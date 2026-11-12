package playlist

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPreviewProxyResolvesLazilyAndPreservesQuery(t *testing.T) {
	var resolved, fetched atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		if r.URL.RawQuery != "time=65.5" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"path":"page.bgra","width":240,"height":135,"count":108,"page":1,"framesPerPage":25,"interval":2}`)
	}))
	defer upstream.Close()
	server, err := Start(testItems(), func(context.Context, int, string) (ResolvedItem, error) {
		resolved.Add(1)
		return ResolvedItem{EDL: "# mpv EDL v0\n", PreviewURL: upstream.URL + "/preview.json"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	endpoint := strings.TrimSuffix(server.URL(), "playlist.m3u") + "p0002-preview.json"
	response, err := http.Head(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 || resolved.Load() != 0 || fetched.Load() != 0 {
		t.Fatalf("HEAD status = %d, resolutions = %d, requests = %d; want 200, 0, 0", response.StatusCode, resolved.Load(), fetched.Load())
	}
	for range 2 {
		response, err := http.Get(endpoint + "?time=65.5")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 || !strings.Contains(string(body), `"page":1`) {
			t.Fatalf("status = %d, body = %s", response.StatusCode, body)
		}
	}
	if resolved.Load() != 1 || fetched.Load() != 2 {
		t.Fatalf("resolutions = %d, requests = %d", resolved.Load(), fetched.Load())
	}
}

func TestPreviewProxyRejectsInvalidResponses(t *testing.T) {
	for _, name := range []string{"redirect", "oversized response", "invalid JSON", "upstream not found", "non-local preview URL", "missing preview URL"} {
		t.Run(name, func(t *testing.T) {
			var unexpected atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/unexpected" {
					unexpected.Store(true)
				}
				switch name {
				case "redirect":
					w.Header().Set("Location", "/unexpected")
					w.WriteHeader(http.StatusTemporaryRedirect)
				case "oversized response":
					_, _ = io.WriteString(w, `"`+strings.Repeat("x", 9000)+`"`)
				case "invalid JSON":
					_, _ = io.WriteString(w, "invalid json")
				case "upstream not found":
					http.NotFound(w, r)
				default:
					t.Error("unexpected upstream request")
				}
			}))
			defer upstream.Close()
			previewURL := upstream.URL + "/preview.json"
			if name == "non-local preview URL" {
				previewURL = "http://example.com/preview.json"
			}
			if name == "missing preview URL" {
				previewURL = ""
			}
			server, err := Start(testItems(), func(context.Context, int, string) (ResolvedItem, error) {
				return ResolvedItem{EDL: "# mpv EDL v0\n", PreviewURL: previewURL}, nil
			}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			response, err := http.Get(strings.TrimSuffix(server.URL(), "playlist.m3u") + "p0001-preview.json")
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			want := http.StatusBadGateway
			if name == "upstream not found" || name == "missing preview URL" {
				want = http.StatusNotFound
			}
			if response.StatusCode != want || unexpected.Load() {
				t.Fatalf("status = %d, redirect followed = %t; want status = %d, redirect followed = false", response.StatusCode, unexpected.Load(), want)
			}
		})
	}
}

func TestPreviewProxyCancelsUpstream(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(canceled) }))
	defer upstream.Close()
	server, err := Start(testItems(), func(context.Context, int, string) (ResolvedItem, error) {
		return ResolvedItem{EDL: "# mpv EDL v0\n", PreviewURL: upstream.URL + "/preview.json"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(server.URL(), "playlist.m3u")+"p0001-preview.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("request error = %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream was not canceled")
	}
}
