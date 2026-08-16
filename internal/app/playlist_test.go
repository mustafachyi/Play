package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"play/internal/media"
	"play/internal/mpv"
	"play/internal/stream"
	"play/internal/youtube"
)

func TestReferencedPlaylistFetchesVideoAndPlaylistConcurrently(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	started := make(chan string, 2)
	release := make(chan struct{})
	deps.resolveVideo = func(context.Context, string) (media.Item, error) {
		started <- "video"
		<-release
		return testItemWithTitle("Selected"), nil
	}
	source := playlistSourceFunc(func(context.Context) ([]media.PlaylistItem, bool, error) {
		started <- "playlist"
		<-release
		return testPlaylistItems(), false, nil
	})

	done := make(chan error, 1)
	go func() {
		_, _, _, _, err := resolveReferencedPlaylist(context.Background(), youtube.Reference{VideoID: "bbbbbbbbbbb", PlaylistID: "PL123"}, source, deps)
		done <- err
	}()
	seen := map[string]bool{}
	for range 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatal("video and playlist resolution did not overlap")
		}
	}
	if !seen["video"] || !seen["playlist"] {
		t.Fatalf("started = %#v", seen)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReferencedPlaylistErrorCancelsConcurrentWork(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	playlistStarted := make(chan struct{})
	playlistCanceled := make(chan struct{})
	source := playlistSourceFunc(func(ctx context.Context) ([]media.PlaylistItem, bool, error) {
		close(playlistStarted)
		<-ctx.Done()
		close(playlistCanceled)
		return nil, false, ctx.Err()
	})
	deps.resolveVideo = func(context.Context, string) (media.Item, error) {
		<-playlistStarted
		return media.Item{}, youtube.ErrLiveUnsupported
	}
	_, _, _, _, err := resolveReferencedPlaylist(context.Background(), youtube.Reference{VideoID: appTestID, PlaylistID: "PL123"}, source, deps)
	if !errors.Is(err, youtube.ErrLiveUnsupported) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-playlistCanceled:
	case <-time.After(time.Second):
		t.Fatal("playlist fetch was not canceled")
	}
}

func TestPlaylistStartsAtReferencedVideoKeepsTitlesAndResolvesLazily(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	var mu sync.Mutex
	var resolved []string
	var got mpv.PlaylistRequest
	deps.resolveVideo = func(ctx context.Context, id string) (media.Item, error) {
		mu.Lock()
		resolved = append(resolved, id)
		mu.Unlock()
		return testItemWithTitle("Resolved " + id), nil
	}
	deps.playPlaylist = func(ctx context.Context, request mpv.PlaylistRequest) error {
		got = request
		resp, err := http.Get(request.URL)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if len(lines) != 7 || lines[3] != "#EXTINF:-1,Resolved bbbbbbbbbbb" {
			return errors.New("playlist titles are incorrect")
		}
		resp, err = http.Get(lines[2])
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return copyErr
	}

	input := "https://www.youtube.com/watch?v=bbbbbbbbbbb&list=PL1234567890&index=2&t=10"
	if err := run(context.Background(), "0.1.0", []string{input}, strings.NewReader(""), io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if got.StartIndex != 1 || got.StartSeconds != 10 || got.AudioOnly {
		t.Fatalf("request = %#v", got)
	}
	mu.Lock()
	gotResolved := append([]string(nil), resolved...)
	mu.Unlock()
	if len(gotResolved) != 2 || gotResolved[0] != "bbbbbbbbbbb" || gotResolved[1] != "aaaaaaaaaaa" {
		t.Fatalf("resolved = %#v", gotResolved)
	}
}

func TestPlaylistSelectedLiveStreamStopsBeforeServers(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	started := false
	played := false
	deps.resolveVideo = func(context.Context, string) (media.Item, error) { return media.Item{}, youtube.ErrLiveUnsupported }
	deps.startDynamicGateway = func(stream.Reporter) (gateway, error) {
		started = true
		return gateway{}, nil
	}
	deps.playPlaylist = func(context.Context, mpv.PlaylistRequest) error {
		played = true
		return nil
	}
	err := run(context.Background(), "0.1.0", []string{"https://youtube.com/playlist?list=PL1234567890"}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if !errors.Is(err, youtube.ErrLiveUnsupported) || started || played {
		t.Fatalf("err=%v started=%v played=%v", err, started, played)
	}
}

func TestPlaylistLaterLiveStreamReportsAndSkipsItem(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	deps.resolveVideo = func(ctx context.Context, id string) (media.Item, error) {
		if id == "bbbbbbbbbbb" {
			return media.Item{}, youtube.ErrLiveUnsupported
		}
		return testItemWithTitle(id), nil
	}
	deps.openPlaylist = func(string) (playlistSource, error) {
		return playlistPages(testPlaylistItems()[:2]), nil
	}
	deps.playPlaylist = func(ctx context.Context, request mpv.PlaylistRequest) error {
		resp, err := http.Get(request.URL)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		resp, err = http.Get(lines[4])
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			return errors.New("live playlist item did not fail")
		}
		return nil
	}
	var diagnostics strings.Builder
	if err := run(context.Background(), "0.1.0", []string{"https://youtube.com/playlist?list=PL1234567890"}, strings.NewReader(""), io.Discard, &diagnostics, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostics.String(), "play: playlist item 2: live streams are not supported") {
		t.Fatalf("diagnostics = %q", diagnostics.String())
	}
}

func TestPlaylistAudioOnlyExposesNativePerFileCover(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	deps.resolveVideo = func(context.Context, string) (media.Item, error) { return testItem(), nil }
	var got mpv.PlaylistRequest
	deps.playPlaylist = func(ctx context.Context, request mpv.PlaylistRequest) error {
		got = request
		resp, err := http.Get(request.URL)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if len(lines) < 3 {
			return errors.New("playlist is incomplete")
		}
		coverURL := strings.Replace(lines[2], "/item/0001.edl", "/cover/0001", 1)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err = client.Get(coverURL)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "http://127.0.0.1:1234/token/p0001-cover.jpg" {
			return errors.New("playlist cover route is incorrect")
		}
		return nil
	}
	if err := run(context.Background(), "0.1.0", []string{"-a", "https://youtube.com/playlist?list=PL1234567890"}, strings.NewReader(""), io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if !got.AudioOnly {
		t.Fatalf("playlist request = %#v", got)
	}
}

func TestPlaylistMixIsRejectedBeforeMPVLookup(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	lookedUp := false
	deps.findMPV = func() (string, error) {
		lookedUp = true
		return "", nil
	}
	err := run(context.Background(), "0.1.0", []string{"https://youtube.com/watch?v=" + appTestID + "&list=RD" + appTestID}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || err.Error() != "YouTube Mix playlists are not supported" || lookedUp {
		t.Fatalf("err=%v lookedUp=%v", err, lookedUp)
	}
}

func TestNamespacePlaybackPlanPreventsPlaylistCollisions(t *testing.T) {
	plan, err := makePlaybackPlan(testItem(), true)
	if err != nil {
		t.Fatal(err)
	}
	namespacePlaybackPlan(&plan, 4)
	for _, resource := range plan.resources {
		if !strings.HasPrefix(resource.Name, "p0005-") {
			t.Fatalf("resource = %q", resource.Name)
		}
	}
	for _, track := range plan.tracks {
		if !strings.HasPrefix(track.name, "p0005-") {
			t.Fatalf("track = %q", track.name)
		}
	}
	if plan.cover != "p0005-cover.jpg" {
		t.Fatalf("cover = %q", plan.cover)
	}
}

func TestPlaylistStartsAfterFirstPageAndLoadsRemainingPagesOnDemand(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	pages := [][]media.PlaylistItem{
		{{VideoID: appTestID, Title: "First"}, {VideoID: "bbbbbbbbbbb", Title: "Second"}},
		{{VideoID: "ccccccccccc", Title: "Third"}},
	}
	calls := 0
	deps.openPlaylist = func(string) (playlistSource, error) {
		return playlistSourceFunc(func(context.Context) ([]media.PlaylistItem, bool, error) {
			page := pages[calls]
			calls++
			return page, calls < len(pages), nil
		}), nil
	}
	deps.resolveVideo = func(context.Context, string) (media.Item, error) { return testItem(), nil }
	deps.playPlaylist = func(ctx context.Context, request mpv.PlaylistRequest) error {
		if calls != 1 {
			return fmt.Errorf("playlist pages fetched before playback = %d", calls)
		}
		if request.PageURL == "" {
			return errors.New("progressive page URL is empty")
		}
		resp, err := http.Get(request.URL)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if strings.Contains(string(body), "Third") {
			return errors.New("initial playlist contains a later page")
		}

		resp, err = http.Get(request.PageURL + "0002.m3u")
		if err != nil {
			return err
		}
		body, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "#EXTINF:-1,Third") || !strings.Contains(string(body), "/item/0003.edl") || calls != 2 {
			return fmt.Errorf("page2 status=%d body=%q calls=%d", resp.StatusCode, body, calls)
		}

		resp, err = http.Get(request.PageURL + "0003.m3u")
		if err != nil {
			return err
		}
		body, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "#EXTM3U" || calls != 2 {
			return fmt.Errorf("terminal page status=%d body=%q calls=%d", resp.StatusCode, body, calls)
		}
		return nil
	}

	if err := run(context.Background(), "0.1.0", []string{"https://youtube.com/playlist?list=PL1234567890"}, strings.NewReader(""), io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
}

func TestPlaylistBackgroundPageFailureIsReported(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	calls := 0
	deps.openPlaylist = func(string) (playlistSource, error) {
		return playlistSourceFunc(func(context.Context) ([]media.PlaylistItem, bool, error) {
			calls++
			if calls == 1 {
				return []media.PlaylistItem{{VideoID: appTestID, Title: "First"}}, true, nil
			}
			return nil, false, errors.New("continuation failed")
		}), nil
	}
	deps.resolveVideo = func(context.Context, string) (media.Item, error) { return testItem(), nil }
	deps.playPlaylist = func(ctx context.Context, request mpv.PlaylistRequest) error {
		resp, err := http.Get(request.PageURL + "0002.m3u")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			return fmt.Errorf("page status = %d", resp.StatusCode)
		}
		return nil
	}
	var diagnostics strings.Builder
	if err := run(context.Background(), "0.1.0", []string{"https://youtube.com/playlist?list=PL1234567890"}, strings.NewReader(""), io.Discard, &diagnostics, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostics.String(), "play: playlist: resolve playlist: continuation failed") {
		t.Fatalf("diagnostics = %q", diagnostics.String())
	}
}

func TestPlaylistStartResolutionFetchesOnlyThroughReferencedPage(t *testing.T) {
	pages := [][]media.PlaylistItem{
		{{VideoID: "aaaaaaaaaaa", Title: "A"}, {VideoID: "bbbbbbbbbbb", Title: "B"}},
		{{VideoID: "ccccccccccc", Title: "C"}, {VideoID: "ddddddddddd", Title: "D"}},
		{{VideoID: "eeeeeeeeeee", Title: "E"}},
	}
	calls := 0
	source := playlistSourceFunc(func(context.Context) ([]media.PlaylistItem, bool, error) {
		page := pages[calls]
		calls++
		return page, calls < len(pages), nil
	})
	items, index, more, err := resolvePlaylistStart(context.Background(), youtubeReference("ddddddddddd", 4), source)
	if err != nil || index != 3 || !more || calls != 2 || len(items) != 4 {
		t.Fatalf("items=%#v index=%d more=%v calls=%d err=%v", items, index, more, calls, err)
	}
}

func TestPlaylistStartIndexUsesIndexThenVideoFallback(t *testing.T) {
	items := testPlaylistItems()
	index, ready, err := playlistStartIndex(items, youtubeReference("bbbbbbbbbbb", 2), true)
	if err != nil || !ready || index != 1 {
		t.Fatalf("index=%d ready=%v err=%v", index, ready, err)
	}
	index, ready, err = playlistStartIndex(items, youtubeReference("ccccccccccc", 2), true)
	if err != nil || !ready || index != 2 {
		t.Fatalf("fallback index=%d ready=%v err=%v", index, ready, err)
	}
	if _, ready, err := playlistStartIndex(items[:1], youtubeReference("ccccccccccc", 3), false); err != nil || ready {
		t.Fatalf("partial playlist ready=%v err=%v", ready, err)
	}
}

func youtubeReference(videoID string, playlistIndex int) youtube.Reference {
	return youtube.Reference{VideoID: videoID, PlaylistID: "PL1234567890", PlaylistIndex: playlistIndex}
}
