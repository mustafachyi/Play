package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"play/internal/media"
	localplaylist "play/internal/playlist"
)

func TestPlaylistGatewayConcurrentPreviewVariantsStress(t *testing.T) {
	var source bytes.Buffer
	artwork := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			artwork.SetRGBA(x, y, color.RGBA{R: uint8(x * 5), G: uint8(y * 10), B: 100, A: 255})
		}
	}
	if err := png.Encode(&source, artwork); err != nil {
		t.Fatal(err)
	}
	var fetched, resolved atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(source.Bytes())
	}))
	defer upstream.Close()
	gateway, err := Start([]Resource{{Name: "preview.json", Preview: &PreviewSource{ArtworkURLs: []string{upstream.URL + "/artwork.png"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	gateway.client.Transport = upstream.Client().Transport
	playlist, err := localplaylist.Start([]media.PlaylistItem{{VideoID: "test-video", Title: "Artwork"}}, func(context.Context, int, string) (localplaylist.ResolvedItem, error) {
		resolved.Add(1)
		return localplaylist.ResolvedItem{EDL: "# mpv EDL v0\n", PreviewURL: gateway.URL("preview.json")}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer playlist.Close()
	endpoint := strings.TrimSuffix(playlist.URL(), "playlist.m3u") + "p0001-preview.json"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	type result struct {
		variant  int
		metadata previewMetadata
		err      error
	}
	const requests = 64
	results := make(chan result, requests)
	start := make(chan struct{})
	for index := range requests {
		go func(variant int) {
			<-start
			width, height := 64+variant*8, 48+variant*4
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s?time=80.5&width=%d&height=%d", endpoint, width, height), nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			response, err := client.Do(request)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer response.Body.Close()
			value := result{variant: variant}
			if response.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(response.Body, 256))
				value.err = fmt.Errorf("status %d: %s", response.StatusCode, body)
			} else {
				value.err = json.NewDecoder(response.Body).Decode(&value.metadata)
			}
			results <- value
		}(index % 8)
	}
	close(start)
	paths := make(map[int]string)
	for range requests {
		select {
		case value := <-results:
			if value.err != nil {
				t.Fatal(value.err)
			}
			metadata := value.metadata
			if metadata.MaxWidth != 64+value.variant*8 || metadata.MaxHeight != 48+value.variant*4 || metadata.Count != 1 || metadata.Page != 0 || metadata.FramesPerPage != 1 || metadata.Width > metadata.MaxWidth || metadata.Height > metadata.MaxHeight {
				t.Fatalf("variant %d metadata = %#v", value.variant, metadata)
			}
			if previous := paths[value.variant]; previous != "" && previous != metadata.Path {
				t.Fatalf("variant %d used multiple cache files", value.variant)
			}
			paths[value.variant] = metadata.Path
			info, err := os.Stat(metadata.Path)
			if err != nil || info.Size() != int64(metadata.Width*metadata.Height*4) {
				t.Fatalf("variant %d packed file = %v, error = %v", value.variant, info, err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent preview variants did not complete:", ctx.Err())
		}
	}
	if len(paths) != 8 || fetched.Load() != 1 || resolved.Load() != 1 {
		t.Fatalf("variants = %d, source fetches = %d, resolutions = %d; want 8, 1, 1", len(paths), fetched.Load(), resolved.Load())
	}
	if err := playlist.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gateway.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("closed gateway left cache file %s: %v", path, err)
		}
	}
}

func TestPlaylistGatewayCloseWithConcurrentPreviewClientsStress(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var startOnce, cancelOnce sync.Once
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			cancelOnce.Do(func() { close(canceled) })
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)
	gateway, err := Start([]Resource{{Name: "preview.json", Preview: &PreviewSource{ArtworkURLs: []string{upstream.URL + "/blocked.png"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	gateway.client.Transport = upstream.Client().Transport
	playlist, err := localplaylist.Start([]media.PlaylistItem{{VideoID: "test-video"}}, func(context.Context, int, string) (localplaylist.ResolvedItem, error) {
		return localplaylist.ResolvedItem{EDL: "# mpv EDL v0\n", PreviewURL: gateway.URL("preview.json")}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer playlist.Close()
	endpoint := strings.TrimSuffix(playlist.URL(), "playlist.m3u") + "p0001-preview.json?width=80&height=60"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	const requests = 24
	done := make(chan struct{}, requests)
	start := make(chan struct{})
	for range requests {
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				return
			}
			response, err := client.Do(request)
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		}()
	}
	close(start)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("upstream preview did not start")
	}
	closed := make(chan error, 2)
	go func() { closed <- playlist.Close() }()
	go func() { closed <- gateway.Close() }()
	for range 2 {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("gateway or playlist close remained blocked")
		}
	}
	for range requests {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("preview client remained blocked after shutdown")
		}
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("shutdown did not cancel the upstream preview")
	}
	gateway.previews.mu.Lock()
	defer gateway.previews.mu.Unlock()
	if !gateway.previews.closed || gateway.previews.directory != "" || gateway.previews.bytes != 0 || len(gateway.previews.entries) != 0 || gateway.previews.image != nil {
		t.Fatal("shutdown retained or published preview resources")
	}
}
