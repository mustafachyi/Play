package playlist

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"play/internal/media"
)

func testItems() []media.PlaylistItem {
	return []media.PlaylistItem{
		{VideoID: "aaaaaaaaaaa", Title: "First title"},
		{VideoID: "bbbbbbbbbbb", Title: "Second title"},
	}
}

func resolved(body string) ResolvedItem {
	return ResolvedItem{EDL: body}
}

func TestServerPublishesTitlesAndResolvesEntriesLazily(t *testing.T) {
	var calls atomic.Int32
	server, err := Start(testItems(), func(ctx context.Context, index int, videoID string) (ResolvedItem, error) {
		calls.Add(1)
		return resolved("# mpv EDL v0\n" + videoID + "\n"), nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	resp, err := http.Get(server.URL())
	if err != nil {
		t.Fatal(err)
	}
	playlistBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("resolver calls = %d", calls.Load())
	}
	lines := strings.Split(strings.TrimSpace(string(playlistBody)), "\n")
	if len(lines) != 5 || lines[0] != "#EXTM3U" || lines[1] != "#EXTINF:-1,First title" || lines[3] != "#EXTINF:-1,Second title" {
		t.Fatalf("playlist = %q", playlistBody)
	}

	for range 2 {
		resp, err = http.Get(lines[4])
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "bbbbbbbbbbb") {
			t.Fatalf("status=%d body=%q", resp.StatusCode, body)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("resolver calls = %d; want 1", calls.Load())
	}
}

func TestPlaylistTitleIsSingleLineAndFallsBackToVideoID(t *testing.T) {
	items := []media.PlaylistItem{{VideoID: "aaaaaaaaaaa", Title: " one\r\n#EXTM3U\x00 "}, {VideoID: "bbbbbbbbbbb"}}
	server, err := Start(items, func(context.Context, int, string) (ResolvedItem, error) { return resolved("# mpv EDL v0\n"), nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	resp, err := http.Get(server.URL())
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if lines[1] != "#EXTINF:-1,one  #EXTM3U" || lines[3] != "#EXTINF:-1,bbbbbbbbbbb" {
		t.Fatalf("playlist = %q", body)
	}
}

func TestPrepareResolvesSelectedEntryOnce(t *testing.T) {
	var calls atomic.Int32
	server, err := Start(testItems(), func(ctx context.Context, index int, videoID string) (ResolvedItem, error) {
		calls.Add(1)
		return resolved("# mpv EDL v0\n" + videoID + "\n"), nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.Prepare(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("resolver calls = %d", calls.Load())
	}
	entryURL := strings.Replace(server.URL(), "playlist.m3u", "item/0002.edl", 1)
	resp, err := http.Get(entryURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestHeadDoesNotResolveEntry(t *testing.T) {
	var calls atomic.Int32
	server, err := Start(testItems()[:1], func(context.Context, int, string) (ResolvedItem, error) {
		calls.Add(1)
		return resolved("# mpv EDL v0\n"), nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	request, err := http.NewRequest(http.MethodHead, strings.Replace(server.URL(), "playlist.m3u", "item/0001.edl", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestCoverRouteUsesResolvedLocalCoverAndSharedCache(t *testing.T) {
	var calls atomic.Int32
	const cover = "http://127.0.0.1:4321/token/cover.jpg"
	server, err := Start(testItems()[:1], func(context.Context, int, string) (ResolvedItem, error) {
		calls.Add(1)
		return ResolvedItem{EDL: "# mpv EDL v0\n", CoverURL: cover}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	coverURL := strings.Replace(server.URL(), "playlist.m3u", "cover/0001", 1)
	resp, err := client.Get(coverURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != cover {
		t.Fatalf("status=%d location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}

	entryURL := strings.Replace(server.URL(), "playlist.m3u", "item/0001.edl", 1)
	resp, err = http.Get(entryURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestCoverRouteReturnsNotFoundWithoutThumbnail(t *testing.T) {
	server, err := Start(testItems()[:1], func(context.Context, int, string) (ResolvedItem, error) {
		return resolved("# mpv EDL v0\n"), nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	resp, err := http.Get(strings.Replace(server.URL(), "playlist.m3u", "cover/0001", 1))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestServerRejectsNonLocalResolvedCover(t *testing.T) {
	server, err := Start(testItems()[:1], func(context.Context, int, string) (ResolvedItem, error) {
		return ResolvedItem{EDL: "# mpv EDL v0\n", CoverURL: "https://example.com/cover.jpg"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	resp, err := http.Get(strings.Replace(server.URL(), "playlist.m3u", "cover/0001", 1))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestServerReportsRepeatedResolutionFailureOncePerMessage(t *testing.T) {
	var reports atomic.Int32
	server, err := Start(testItems()[:1], func(context.Context, int, string) (ResolvedItem, error) {
		return ResolvedItem{}, errors.New("unsupported item")
	}, nil, func(index int, err error) {
		if index != 0 || err.Error() != "unsupported item" {
			t.Fatalf("report index=%d err=%v", index, err)
		}
		reports.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	entryURL := strings.Replace(server.URL(), "playlist.m3u", "item/0001.edl", 1)
	for range 2 {
		resp, err := http.Get(entryURL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	if reports.Load() != 1 {
		t.Fatalf("reports = %d; want 1", reports.Load())
	}
}

func TestConcurrentEntryAndCoverRequestsResolveOnce(t *testing.T) {
	var calls atomic.Int32
	server, err := Start(testItems()[:1], func(context.Context, int, string) (ResolvedItem, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return ResolvedItem{EDL: "# mpv EDL v0\n", CoverURL: "http://127.0.0.1:4321/token/cover.jpg"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	urls := []string{
		strings.Replace(server.URL(), "playlist.m3u", "item/0001.edl", 1),
		strings.Replace(server.URL(), "playlist.m3u", "cover/0001", 1),
	}
	var wg sync.WaitGroup
	wg.Add(len(urls))
	for _, target := range urls {
		go func() {
			defer wg.Done()
			resp, err := client.Get(target)
			if err != nil {
				t.Errorf("GET: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("resolver calls = %d; want 1", calls.Load())
	}
}

func TestProgressivePagesAppendRoutesInOrderAndCacheResults(t *testing.T) {
	pages := [][]media.PlaylistItem{
		{{VideoID: "ccccccccccc", Title: "Third title"}},
		{{VideoID: "ddddddddddd", Title: "Fourth title"}},
	}
	var loads atomic.Int32
	pageLoader := func(context.Context) ([]media.PlaylistItem, bool, error) {
		index := int(loads.Add(1)) - 1
		return pages[index], index+1 < len(pages), nil
	}
	server, err := Start(testItems(), func(ctx context.Context, index int, videoID string) (ResolvedItem, error) {
		return resolved("# mpv EDL v0\n" + videoID + "\n"), nil
	}, pageLoader, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if server.PageURL() == "" {
		t.Fatal("page URL is empty")
	}

	page2 := server.PageURL() + "0002.m3u"
	resp, err := http.Get(page2)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "#EXTINF:-1,Third title") || !strings.Contains(string(body), "/item/0003.edl") || loads.Load() != 1 {
		t.Fatalf("page2 status=%d body=%q loads=%d", resp.StatusCode, body, loads.Load())
	}

	resp, err = http.Get(page2)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(cached) != string(body) || loads.Load() != 1 {
		t.Fatalf("cached=%q loads=%d", cached, loads.Load())
	}

	resp, err = http.Get(server.PageURL() + "0003.m3u")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "#EXTINF:-1,Fourth title") || !strings.Contains(string(body), "/item/0004.edl") || loads.Load() != 2 {
		t.Fatalf("page3 status=%d body=%q loads=%d", resp.StatusCode, body, loads.Load())
	}

	resp, err = http.Get(server.PageURL() + "0004.m3u")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "#EXTM3U" || loads.Load() != 2 {
		t.Fatalf("terminal status=%d body=%q loads=%d", resp.StatusCode, body, loads.Load())
	}

	resp, err = http.Get(strings.Replace(server.URL(), "playlist.m3u", "item/0004.edl", 1))
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ddddddddddd") {
		t.Fatalf("item status=%d body=%q", resp.StatusCode, body)
	}
}

func TestProgressivePageRequestsAreSequentialAndHeadDoesNotAdvance(t *testing.T) {
	var loads atomic.Int32
	server, err := Start(testItems()[:1], func(context.Context, int, string) (ResolvedItem, error) {
		return resolved("# mpv EDL v0\n"), nil
	}, func(context.Context) ([]media.PlaylistItem, bool, error) {
		loads.Add(1)
		return []media.PlaylistItem{{VideoID: "bbbbbbbbbbb", Title: "Second"}}, false, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	head, err := http.NewRequest(http.MethodHead, server.PageURL()+"0002.m3u", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(head)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || loads.Load() != 0 {
		t.Fatalf("HEAD status=%d loads=%d", resp.StatusCode, loads.Load())
	}

	resp, err = http.Get(server.PageURL() + "0003.m3u")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || loads.Load() != 0 {
		t.Fatalf("out-of-sequence status=%d loads=%d", resp.StatusCode, loads.Load())
	}
}

func TestProgressivePageLoadUsesRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	server, err := Start(testItems()[:1], func(context.Context, int, string) (ResolvedItem, error) {
		return resolved("# mpv EDL v0\n"), nil
	}, func(ctx context.Context) ([]media.PlaylistItem, bool, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil, false, ctx.Err()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.PageURL()+"0002.m3u", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("page loader context was not canceled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("page request did not terminate")
	}
}

func TestCloseCancelsActiveResolution(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	server, err := Start(testItems()[:1], func(ctx context.Context, index int, videoID string) (ResolvedItem, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ResolvedItem{}, ctx.Err()
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	entryURL := strings.Replace(server.URL(), "playlist.m3u", "item/0001.edl", 1)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, err := http.Get(entryURL)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("resolver context was not canceled")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("client request did not terminate")
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestStartRejectsInvalidInput(t *testing.T) {
	if _, err := Start(nil, func(context.Context, int, string) (ResolvedItem, error) { return ResolvedItem{}, nil }, nil, nil); err == nil {
		t.Fatal("expected empty playlist to be rejected")
	}
	if _, err := Start(testItems()[:1], nil, nil, nil); err == nil {
		t.Fatal("expected nil resolver to be rejected")
	}
	if _, err := Start([]media.PlaylistItem{{Title: "missing ID"}}, func(context.Context, int, string) (ResolvedItem, error) { return ResolvedItem{}, nil }, nil, nil); err == nil {
		t.Fatal("expected empty video ID to be rejected")
	}
}
