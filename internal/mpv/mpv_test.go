package mpv

import (
	"reflect"
	"strings"
	"testing"
)

func testRequest() Request {
	return Request{
		Executable:   `C:\mpv\mpv.exe`,
		Title:        "Actual title",
		Duration:     801,
		StartSeconds: 462,
		Tracks: []Track{
			{Kind: Video, URL: "http://127.0.0.1:1234/token/v01.webm", Title: "2160p", MIME: `video/webm; codecs="vp09.00.51.08"`, Width: 3840, Height: 2160, FPS: 60, Bitrate: 20_000_000},
			{Kind: Video, URL: "http://127.0.0.1:1234/token/v02.webm", Title: "1080p", MIME: `video/webm; codecs="vp9"`, Width: 1920, Height: 1080, FPS: 60, Bitrate: 8_000_000, Default: true},
			{Kind: Audio, URL: "http://127.0.0.1:1234/token/a01.webm", Title: "English (US) original", Language: "en-US", MIME: `audio/webm; codecs="opus"`, SampleRate: 48000, Bitrate: 128000, Default: true},
			{Kind: Subtitle, URL: "http://127.0.0.1:1234/token/s01.vtt", Title: "English (auto-generated)", Language: "en", MIME: "text/vtt", Default: true},
		},
	}
}

func TestBuildEDLUsesDelayedTracksMetadataAndDefaults(t *testing.T) {
	request := testRequest()
	request.CoverURL = "http://127.0.0.1:1234/token/cover.jpg"
	edl := buildEDL(request)
	if !strings.HasPrefix(edl, "# mpv EDL v0\n") {
		t.Fatalf("EDL header missing: %q", edl)
	}
	if strings.Count(edl, "!delay_open") != 4 || strings.Count(edl, "!new_stream") != 4 {
		t.Fatalf("EDL does not expose all tracks lazily:\n%s", edl)
	}
	for _, want := range []string{
		"!global_tags,title=%12%Actual title",
		"!delay_open,media_type=video,codec=vp9,w=1920,h=1080,fps=60",
		"!track_meta,title=%5%1080p,byterate=1000000,flags=default",
		"!delay_open,media_type=audio,codec=opus,samplerate=48000",
		"!track_meta,title=%21%English (US) original,lang=%5%en-US,byterate=16000,flags=default",
		"!delay_open,media_type=sub,codec=webvtt",
		"!track_meta,title=%24%English (auto-generated),lang=%2%en,flags=default",
		"length=801",
	} {
		if !strings.Contains(edl, want) {
			t.Fatalf("EDL missing %q:\n%s", want, edl)
		}
	}
	if strings.Count(edl, "flags=default") != 3 {
		t.Fatalf("default flag count = %d", strings.Count(edl, "flags=default"))
	}
	for _, line := range strings.Split(edl, "\n") {
		if strings.HasPrefix(line, "!delay_open") && strings.Contains(line, "flags=default") {
			t.Fatalf("delay_open contains default flag: %q", line)
		}
	}
	if strings.Contains(edl, "play-cover-url") {
		t.Fatalf("EDL contains obsolete cover metadata:\n%s", edl)
	}
}

func TestEDLDoesNotRequireExecutable(t *testing.T) {
	request := testRequest()
	request.Executable = ""
	if _, err := EDL(request); err != nil {
		t.Fatalf("EDL rejected media-only request: %v", err)
	}
}

func TestVideoArgumentsKeepWindowAndSelectionStable(t *testing.T) {
	got := arguments(testRequest(), `C:\Temp\play-123.edl`)
	want := []string{
		"--terminal=no",
		"--ytdl=no",
		"--vid=auto",
		"--aid=1",
		"--sid=auto",
		"--sub-visibility=no",
		"--watch-later-options-remove=sub-pos",
		"--force-media-title=Actual title",
		`--input-commands-append=keybind _ "cycle-values video 1 2"`,
		"--start=462",
		"--",
		`C:\Temp\play-123.edl`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %#v; want %#v", got, want)
	}
}

func TestAudioOnlyArgumentsForceVisibleWindow(t *testing.T) {
	request := Request{
		Executable: `C:\mpv\mpv.exe`, Title: "Audio only", Duration: 300,
		Tracks: []Track{
			{Kind: Audio, URL: "http://127.0.0.1:1234/token/a01.webm", Title: "English", Language: "en", MIME: `audio/webm; codecs="opus"`, Default: true},
		},
		CoverURL: "http://127.0.0.1:1234/token/cover.jpg",
	}
	got := arguments(request, `C:\Temp\play-456.edl`)
	for _, want := range []string{"--vid=auto", "--aid=1", "--sid=auto", "--sub-visibility=no", "--force-window=yes", "--force-media-title=Audio only", "--cover-art-file=http://127.0.0.1:1234/token/cover.jpg", "--audio-display=external-first"} {
		if !contains(got, want) {
			t.Fatalf("arguments missing %q: %#v", want, got)
		}
	}
}

func TestAudioOnlyWithoutCoverStillForcesWindow(t *testing.T) {
	request := Request{Executable: `C:\mpv\mpv.exe`, Title: "Audio only", Tracks: []Track{{Kind: Audio, URL: "http://127.0.0.1:1/a", Title: "Audio"}}}
	got := arguments(request, `C:\Temp\play.edl`)
	if !contains(got, "--force-window=yes") || contains(got, "--audio-display=external-first") {
		t.Fatalf("arguments = %#v", got)
	}
}

func TestPlaylistAudioOnlyForcesWindowAndCoverSelection(t *testing.T) {
	request := PlaylistRequest{Executable: `C:\mpv\mpv.exe`, URL: "http://127.0.0.1:1234/token/playlist.m3u", StartIndex: 3, StartSeconds: 42, AudioOnly: true}
	got := playlistArguments(request, `C:\Temp\play.lua`)
	for _, want := range []string{"--vid=auto", "--aid=1", "--sid=auto", "--sub-visibility=no", "--force-window=yes", "--audio-display=external-first", "--playlist-start=3", "--playlist=http://127.0.0.1:1234/token/playlist.m3u"} {
		if !contains(got, want) {
			t.Fatalf("arguments missing %q: %#v", want, got)
		}
	}
}

func TestPlaylistVideoArgumentsUseDefaultTrackSelection(t *testing.T) {
	request := PlaylistRequest{Executable: `C:\mpv\mpv.exe`, URL: "http://127.0.0.1:1234/token/playlist.m3u", StartIndex: 3}
	got := playlistArguments(request, `C:\Temp\play.lua`)
	if !contains(got, "--vid=auto") || !contains(got, "--sid=auto") || !contains(got, "--sub-visibility=no") || contains(got, "--force-window=yes") || contains(got, "--audio-display=external-first") {
		t.Fatalf("arguments = %#v", got)
	}
}

func TestPlaylistScriptAppliesPerItemTitleCoverAndStartBeforeLoad(t *testing.T) {
	script := playlistScript(2, 90, true, "http://127.0.0.1:1234/token/page/")
	for _, want := range []string{
		`track.type == "video" and not track.albumart`,
		`mp.add_key_binding("_", "play-cycle-video", cycle_video)`,
		`mp.set_property_number("vid", ids[next_index])`,
		`local audio_only = true`,
		`mp.add_hook("on_load", 50`,
		`playlist-playing-pos`,
		`playlist/" .. tostring(playlist_pos) .. "/title`,
		`stream-open-filename`,
		`/item/(%d+)%.edl$`,
		`/cover/%1`,
		`file-local-options/cover-art-files`,
		`start_index = 2`,
		`start_seconds = 90`,
		`file-local-options/start`,
		`mp.add_hook("on_preloaded", 50`,
		`metadata/by-key/title`,
		`file-local-options/force-media-title`,
		`local page_url = "http://127.0.0.1:1234/token/page/"`,
		`mp.register_event("file-loaded", start_pagination)`,
		`mp.command_native_async({"loadlist", url, "append"}`,
		`playlist-count`,
		`string.format("%s%04d.m3u", page_url, next_page)`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	for _, obsolete := range []string{"video-add", "attached-picture", "play-cover-url"} {
		if strings.Contains(script, obsolete) {
			t.Fatalf("script contains obsolete cover path %q:\n%s", obsolete, script)
		}
	}
	if strings.Contains(script, `set_property("vid", "no")`) {
		t.Fatal("script can disable video")
	}
}

func TestPlaylistVideoScriptDoesNotEnableAudioCoverPath(t *testing.T) {
	script := playlistScript(0, 0, false, "")
	if !strings.Contains(script, `local audio_only = false`) {
		t.Fatalf("script = %s", script)
	}
}

func TestCodecHint(t *testing.T) {
	tests := []struct {
		track Track
		want  string
	}{
		{Track{Kind: Video, MIME: `video/mp4; codecs="avc1.640028"`}, "h264"},
		{Track{Kind: Video, MIME: `video/webm; codecs="vp09.00.51.08"`}, "vp9"},
		{Track{Kind: Video, MIME: `video/mp4; codecs="av01.0.08M.08"`}, "av1"},
		{Track{Kind: Audio, MIME: `audio/mp4; codecs="mp4a.40.2"`}, "aac"},
		{Track{Kind: Audio, MIME: `audio/webm; codecs="opus"`}, "opus"},
		{Track{Kind: Subtitle, MIME: "text/vtt"}, "webvtt"},
		{Track{Kind: Video, MIME: "video/unknown"}, "null"},
	}
	for _, test := range tests {
		if got := codecHint(test.track); got != test.want {
			t.Fatalf("codecHint(%q) = %q; want %q", test.track.MIME, got, test.want)
		}
	}
}

func TestValidationRejectsNonLocalURLs(t *testing.T) {
	request := testRequest()
	request.Tracks[0].URL = "https://rr.googlevideo.com/video"
	if err := validateRequest(request); err == nil {
		t.Fatal("expected direct upstream URL to be rejected")
	}
	if err := validatePlaylistRequest(PlaylistRequest{Executable: `C:\mpv\mpv.exe`, URL: "https://youtube.com/playlist"}); err == nil {
		t.Fatal("expected non-local playlist to be rejected")
	}
	if err := validatePlaylistRequest(PlaylistRequest{Executable: `C:\mpv\mpv.exe`, URL: "http://127.0.0.1:1/playlist.m3u", PageURL: "https://youtube.com/page/"}); err == nil {
		t.Fatal("expected non-local playlist page URL to be rejected")
	}
}

func TestLocalHTTPURL(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"http://127.0.0.1:1234/token/video", true},
		{"http://[::1]:1234/token/video", true},
		{"https://127.0.0.1:1234/token/video", false},
		{"http://example.com:1234/video", false},
		{"http://127.0.0.1/video", false},
	}
	for _, test := range tests {
		if got := localHTTPURL(test.value); got != test.want {
			t.Fatalf("localHTTPURL(%q) = %v; want %v", test.value, got, test.want)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestMediaRequestValidationRejectsInvalidFields(t *testing.T) {
	base := testRequest()
	cases := []Request{
		{Executable: base.Executable, Tracks: base.Tracks},
		{Executable: base.Executable, Title: base.Title},
		{Executable: base.Executable, Title: base.Title, Tracks: base.Tracks, Duration: -1},
	}
	for _, request := range cases {
		if err := validateRequest(request); err == nil {
			t.Fatalf("request unexpectedly valid: %#v", request)
		}
	}
	request := base
	request.Tracks[0].Kind = "bad"
	if err := validateRequest(request); err == nil {
		t.Fatal("invalid track kind accepted")
	}
	request = base
	request.CoverURL = "https://example.com/cover.jpg"
	if err := validateRequest(request); err == nil {
		t.Fatal("non-local cover URL accepted")
	}
}
