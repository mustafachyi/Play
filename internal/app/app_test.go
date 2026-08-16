package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"play/internal/media"
	"play/internal/mpv"
	"play/internal/stream"
	"play/internal/youtube"
)

const appTestID = "dQw4w9WgXcQ"

type playlistSourceFunc func(context.Context) ([]media.PlaylistItem, bool, error)

func (f playlistSourceFunc) Next(ctx context.Context) ([]media.PlaylistItem, bool, error) {
	return f(ctx)
}

func playlistPages(pages ...[]media.PlaylistItem) playlistSource {
	index := 0
	return playlistSourceFunc(func(context.Context) ([]media.PlaylistItem, bool, error) {
		if index >= len(pages) {
			return nil, false, nil
		}
		page := pages[index]
		index++
		return page, index < len(pages), nil
	})
}

func testItem() media.Item {
	return testItemWithTitle("Test video")
}

func testItemWithTitle(title string) media.Item {
	return media.Item{
		Title: title, Duration: 801, Thumbnail: "https://image/cover.jpg",
		Videos: []media.Video{
			{Stream: media.Stream{URL: "https://video/2160", MIME: `video/webm; codecs="vp9"`, Size: 21_600_000}, Quality: "2160p", Width: 3840, Height: 2160, FPS: 60, Bitrate: 20_000_000},
			{Stream: media.Stream{URL: "https://video/1080", MIME: `video/webm; codecs="vp9"`, Size: 10_800_000}, Quality: "1080p", Width: 1920, Height: 1080, FPS: 60, Bitrate: 8_000_000},
			{Stream: media.Stream{URL: "https://video/720", MIME: `video/mp4; codecs="avc1.64001f"`, Size: 7_200_000}, Quality: "720p", Width: 1280, Height: 720, FPS: 60, Bitrate: 5_000_000},
		},
		Audios: []media.Audio{
			{Stream: media.Stream{URL: "https://audio/fr", MIME: `audio/mp4; codecs="mp4a.40.2"`, Size: 111_111}, Language: "French (France)", LanguageCode: "fr-FR", Default: true, TrackID: "fr-FR.1", SampleRate: 44100, Bitrate: 128000},
			{Stream: media.Stream{URL: "https://audio/en", MIME: `audio/webm; codecs="opus"`, Size: 222_222}, Language: "English (US) original", LanguageCode: "en-US", TrackID: "en-US.1", SampleRate: 48000, Bitrate: 128000},
		},
		Subtitles: []media.Subtitle{
			{URL: "https://subtitle/en?fmt=vtt", Language: "English (auto-generated)", LanguageCode: "en", Auto: true},
			{URL: "https://subtitle/es?fmt=vtt", Language: "Spanish", LanguageCode: "es"},
		},
	}
}

func testPlaylistItems() []media.PlaylistItem {
	return []media.PlaylistItem{
		{VideoID: "aaaaaaaaaaa", Title: "Browse A"},
		{VideoID: "bbbbbbbbbbb", Title: "Browse B"},
		{VideoID: "ccccccccccc", Title: "Browse C"},
	}
}

func testDependencies(item media.Item, resources *[]stream.Resource, request *mpv.Request, closed *bool) dependencies {
	newGateway := func(values []stream.Resource) (gateway, error) {
		names := make(map[string]struct{})
		add := func(values []stream.Resource) error {
			for _, resource := range values {
				if _, exists := names[resource.Name]; exists {
					return errors.New("duplicate resource")
				}
			}
			for _, resource := range values {
				names[resource.Name] = struct{}{}
			}
			if resources != nil {
				*resources = append(*resources, values...)
			}
			return nil
		}
		if err := add(values); err != nil {
			return gateway{}, err
		}
		return gateway{
			url: func(name string) string {
				if _, ok := names[name]; !ok {
					return ""
				}
				return "http://127.0.0.1:1234/token/" + name
			},
			add: add,
			close: func() error {
				if closed != nil {
					*closed = true
				}
				return nil
			},
		}, nil
	}
	return dependencies{
		resolveVideo: func(ctx context.Context, videoID string) (media.Item, error) {
			if videoID != appTestID {
				return media.Item{}, errors.New("wrong video ID")
			}
			return item, nil
		},
		openPlaylist: func(string) (playlistSource, error) { return playlistPages(testPlaylistItems()), nil },
		findMPV:      func() (string, error) { return `C:\mpv\mpv.exe`, nil },
		startGateway: func(values []stream.Resource, reporter stream.Reporter) (gateway, error) {
			return newGateway(values)
		},
		startDynamicGateway: func(stream.Reporter) (gateway, error) { return newGateway(nil) },
		play: func(ctx context.Context, value mpv.Request) error {
			if request != nil {
				*request = value
			}
			return nil
		},
		playPlaylist: func(context.Context, mpv.PlaylistRequest) error { return nil },
	}
}

func TestDirectModeExposesTrackSetWithDefaultsAndTimestamp(t *testing.T) {
	var resources []stream.Resource
	var request mpv.Request
	var closed bool
	deps := testDependencies(testItem(), &resources, &request, &closed)
	input := "https://www.youtube.com/watch?v=" + appTestID + "&t=7m42s"
	var out strings.Builder
	if err := run(context.Background(), "0.3.0", []string{input}, strings.NewReader(""), &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || request.StartSeconds != 462 || request.Duration != 801 || request.Title != "Test video" || request.Executable == "" {
		t.Fatalf("out=%q request=%#v", out.String(), request)
	}
	if len(resources) != 7 || len(request.Tracks) != 7 {
		t.Fatalf("resources=%d tracks=%d", len(resources), len(request.Tracks))
	}
	if request.Tracks[0].Kind != mpv.Video || request.Tracks[0].Title != "2160p" || request.Tracks[0].Default {
		t.Fatalf("first video = %#v", request.Tracks[0])
	}
	if request.Tracks[1].Kind != mpv.Video || request.Tracks[1].Title != "1080p" || !request.Tracks[1].Default {
		t.Fatalf("default video = %#v", request.Tracks[1])
	}
	if request.Tracks[3].Kind != mpv.Audio || request.Tracks[3].Title != "English (US) original" || !request.Tracks[3].Default {
		t.Fatalf("first audio = %#v", request.Tracks[3])
	}
	if request.Tracks[5].Kind != mpv.Subtitle || request.Tracks[5].Title != "English (auto-generated)" || !request.Tracks[5].Default || request.Tracks[6].Default {
		t.Fatalf("subtitle defaults = %#v %#v", request.Tracks[5], request.Tracks[6])
	}
	if resources[0].Name != "v01.webm" || resources[3].Name != "a01.webm" || resources[5].Name != "s01.vtt" || request.CoverURL != "" || !closed {
		t.Fatalf("resources=%#v cover=%q closed=%v", resources, request.CoverURL, closed)
	}
}

func TestVideoTrackOrderIsIndependentOfDefaultSelection(t *testing.T) {
	tests := []struct {
		name           string
		videos         []media.Video
		defaultTitle   string
		expectedTitles []string
	}{
		{
			name: "1080 available",
			videos: []media.Video{
				{Stream: media.Stream{URL: "https://video/2160"}, Quality: "2160p", Height: 2160},
				{Stream: media.Stream{URL: "https://video/1440"}, Quality: "1440p", Height: 1440},
				{Stream: media.Stream{URL: "https://video/1080"}, Quality: "1080p", Height: 1080},
				{Stream: media.Stream{URL: "https://video/720"}, Quality: "720p", Height: 720},
				{Stream: media.Stream{URL: "https://video/480"}, Quality: "480p", Height: 480},
				{Stream: media.Stream{URL: "https://video/360"}, Quality: "360p", Height: 360},
			},
			defaultTitle:   "1080p",
			expectedTitles: []string{"2160p", "1440p", "1080p", "720p", "480p", "360p"},
		},
		{
			name: "fallback below 1080",
			videos: []media.Video{
				{Stream: media.Stream{URL: "https://video/2160"}, Quality: "2160p", Height: 2160},
				{Stream: media.Stream{URL: "https://video/1440"}, Quality: "1440p", Height: 1440},
				{Stream: media.Stream{URL: "https://video/720"}, Quality: "720p", Height: 720},
				{Stream: media.Stream{URL: "https://video/480"}, Quality: "480p", Height: 480},
			},
			defaultTitle:   "720p",
			expectedTitles: []string{"2160p", "1440p", "720p", "480p"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := testItem()
			item.Videos = test.videos
			plan, err := makePlaybackPlan(item, false)
			if err != nil {
				t.Fatal(err)
			}
			var tracks []mpv.Track
			for _, planned := range plan.tracks {
				if planned.track.Kind == mpv.Video {
					tracks = append(tracks, planned.track)
				}
			}
			if len(tracks) != len(test.expectedTitles) {
				t.Fatalf("video tracks = %#v", tracks)
			}
			for i, title := range test.expectedTitles {
				if tracks[i].Title != title || tracks[i].Default != (title == test.defaultTitle) {
					t.Fatalf("track %d = %#v; want title=%q default=%v", i, tracks[i], title, title == test.defaultTitle)
				}
			}
		})
	}
}

func TestInteractiveModePromptsForReference(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	var out strings.Builder
	if err := run(context.Background(), "0.3.0", nil, strings.NewReader(appTestID+"\n"), &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if out.String() != "YouTube URL, playlist URL, or video ID: " {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAudioOnlyUsesPreferredAudioThumbnailAndNoVideo(t *testing.T) {
	var resources []stream.Resource
	var request mpv.Request
	deps := testDependencies(testItem(), &resources, &request, nil)
	if err := run(context.Background(), "0.3.0", []string{"-a", appTestID}, strings.NewReader(""), io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if len(request.Tracks) != 4 || request.Tracks[0].Kind != mpv.Audio || request.Tracks[0].Title != "English (US) original" || !request.Tracks[0].Default {
		t.Fatalf("tracks = %#v", request.Tracks)
	}
	if request.Tracks[2].Kind != mpv.Subtitle || !request.Tracks[2].Default || request.Tracks[3].Default {
		t.Fatalf("subtitle defaults = %#v %#v", request.Tracks[2], request.Tracks[3])
	}
	for _, track := range request.Tracks {
		if track.Kind == mpv.Video {
			t.Fatalf("audio-only request contains video: %#v", track)
		}
	}
	if request.CoverURL != "http://127.0.0.1:1234/token/cover.jpg" || len(resources) != 5 {
		t.Fatalf("cover=%q resources=%#v", request.CoverURL, resources)
	}
}

func TestAudioOnlyDoesNotFailWithoutThumbnail(t *testing.T) {
	item := testItem()
	item.Thumbnail = ""
	var request mpv.Request
	deps := testDependencies(item, nil, &request, nil)
	if err := run(context.Background(), "0.3.0", []string{"-a", appTestID}, strings.NewReader(""), io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if request.CoverURL != "" {
		t.Fatalf("cover URL = %q", request.CoverURL)
	}
}

func TestNormalModeNeverSelectsAbove1080(t *testing.T) {
	item := testItem()
	item.Videos = item.Videos[:1]
	started := false
	deps := testDependencies(item, nil, nil, nil)
	deps.startGateway = func([]stream.Resource, stream.Reporter) (gateway, error) {
		started = true
		return gateway{}, nil
	}
	err := run(context.Background(), "0.3.0", []string{appTestID}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "at or below 1080p") || started {
		t.Fatalf("err=%v started=%v", err, started)
	}
}

func TestGatewayClosesWhenPlayerFails(t *testing.T) {
	var closed bool
	deps := testDependencies(testItem(), nil, nil, &closed)
	deps.play = func(context.Context, mpv.Request) error { return errors.New("player failed") }
	err := run(context.Background(), "0.3.0", []string{appTestID}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if err == nil || !strings.Contains(err.Error(), "player failed") || !closed {
		t.Fatalf("err=%v closed=%v", err, closed)
	}
}

func TestDirectLiveStreamReturnsClearErrorBeforeGateway(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	started := false
	deps.resolveVideo = func(context.Context, string) (media.Item, error) { return media.Item{}, youtube.ErrLiveUnsupported }
	deps.startGateway = func([]stream.Resource, stream.Reporter) (gateway, error) {
		started = true
		return gateway{}, nil
	}
	err := run(context.Background(), "0.1.0", []string{appTestID}, strings.NewReader(""), io.Discard, io.Discard, deps)
	if !errors.Is(err, youtube.ErrLiveUnsupported) || started {
		t.Fatalf("err=%v started=%v", err, started)
	}
}

func TestSingleDashOptions(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	called := false
	deps.resolveVideo = func(context.Context, string) (media.Item, error) {
		called = true
		return media.Item{}, nil
	}
	var out strings.Builder
	if err := run(context.Background(), "1.2.3", []string{"-version"}, strings.NewReader(""), &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if out.String() != "play 1.2.3\n" || called {
		t.Fatalf("output=%q called=%v", out.String(), called)
	}
	out.Reset()
	if err := run(context.Background(), "1.2.3", []string{"-h"}, strings.NewReader(""), &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "playlist-url") || called {
		t.Fatalf("output=%q called=%v", out.String(), called)
	}
	if err := run(context.Background(), "1.2.3", []string{"--version"}, strings.NewReader(""), io.Discard, io.Discard, deps); err == nil {
		t.Fatal("expected double-dash option to be rejected")
	}
}

func TestParseArgsAcceptsAudioFlagBeforeOrAfterInput(t *testing.T) {
	for _, args := range [][]string{{"-a", appTestID}, {appTestID, "-a"}} {
		opts, err := parseArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		if !opts.audioOnly || opts.input != appTestID {
			t.Fatalf("args=%#v opts=%#v", args, opts)
		}
	}
}

func TestParseArgsAcceptsVideoIDStartingWithHyphen(t *testing.T) {
	const id = "-abcdefghij"
	opts, err := parseArgs([]string{id})
	if err != nil || opts.input != id {
		t.Fatalf("opts=%#v err=%v", opts, err)
	}
}
