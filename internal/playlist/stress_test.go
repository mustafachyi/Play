package playlist

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"play/internal/media"
)

func stressPlaylistItems() []media.PlaylistItem {
	items := make([]media.PlaylistItem, 5000)
	for index := range items {
		items[index] = media.PlaylistItem{VideoID: fmt.Sprintf("%011d", index), Title: strings.Repeat("Long title 日本語 ", 8)}
	}
	return items
}

func playlistRequestBurst(t *testing.T, client *http.Client, targets []string, statuses []int) {
	t.Helper()
	start := make(chan struct{})
	failures := make(chan error, len(targets))
	var wait sync.WaitGroup
	for index, target := range targets {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			response, err := client.Get(target)
			if err != nil {
				failures <- err
				return
			}
			_, readErr := io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if readErr != nil {
				failures <- readErr
			} else if response.StatusCode != statuses[index] {
				failures <- fmt.Errorf("status = %d; want %d", response.StatusCode, statuses[index])
			}
		}()
	}
	close(start)
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

func TestFiveThousandItemPlaylistConcurrentPaginationAndLazyResolution(t *testing.T) {
	items := stressPlaylistItems()
	var pages, resolutions atomic.Int32
	server, err := Start(items[:500], func(_ context.Context, index int, videoID string) (ResolvedItem, error) {
		resolutions.Add(1)
		if index != 4999 || videoID != items[4999].VideoID {
			return ResolvedItem{}, fmt.Errorf("unexpected eager item resolution: %d, %s", index, videoID)
		}
		return ResolvedItem{EDL: "# mpv EDL v0\n", CoverURL: "http://127.0.0.1:1/token/cover.jpg"}, nil
	}, func(context.Context) ([]media.PlaylistItem, bool, error) {
		start := int(pages.Add(1)) * 500
		if start >= len(items) {
			return nil, false, fmt.Errorf("page loader exceeded available items")
		}
		end := start + 500
		return items[start:end], end < len(items), nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	targets, statuses := make([]string, 32), make([]int, 32)
	for index := range targets {
		targets[index], statuses[index] = server.PageURL()+"0002.m3u", http.StatusOK
	}
	playlistRequestBurst(t, client, targets, statuses)
	if pages.Load() != 1 {
		t.Fatalf("concurrent page loader calls = %d; want 1", pages.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for page := 3; page <= 10; page++ {
		if _, err := server.loadPage(ctx, page); err != nil {
			t.Fatal(err)
		}
	}
	if len(server.entries) != 5000 || len(server.routes) != 15000 || len(server.pageBodies) != 9 || pages.Load() != 9 || resolutions.Load() != 0 {
		t.Fatalf("entries = %d, routes = %d, cached pages = %d, loader calls = %d, eager resolutions = %d", len(server.entries), len(server.routes), len(server.pageBodies), pages.Load(), resolutions.Load())
	}
	for index := range targets {
		if index%2 == 0 {
			targets[index], statuses[index] = strings.Replace(server.URL(), "playlist.m3u", "item/5000.edl", 1), http.StatusOK
		} else {
			targets[index], statuses[index] = strings.Replace(server.URL(), "playlist.m3u", "cover/5000", 1), http.StatusTemporaryRedirect
		}
	}
	playlistRequestBurst(t, client, targets, statuses)
	if resolutions.Load() != 1 {
		t.Fatalf("concurrent item/cover resolver calls = %d; want 1", resolutions.Load())
	}
}
