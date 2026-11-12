package mpv

import (
	"strings"
	"testing"
)

func TestPreviewArgumentsRequireLocalURL(t *testing.T) {
	request := testRequest()
	request.PreviewURL = "http://127.0.0.1:1234/token/preview.json"
	if err := validateMediaRequest(request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(arguments(request, "test.edl"), "\n"), "--script-opts-append=playview-preview="+request.PreviewURL) {
		t.Fatal("preview argument is missing")
	}
	for _, value := range []string{"https://example.com/preview", "http://example.com/preview", "file:///tmp/preview"} {
		request.PreviewURL = value
		if _, err := EDL(request); err == nil {
			t.Fatalf("non-local preview URL was accepted: %q", value)
		}
	}
}

func TestPlaylistArgumentsAdvertisePreviewSupport(t *testing.T) {
	for _, audioOnly := range []bool{false, true} {
		request := PlaylistRequest{URL: "http://127.0.0.1:1234/token/playlist.m3u", AudioOnly: audioOnly}
		if !contains(playlistArguments(request, "test.lua"), "--script-opts-append=playview-preview=playlist") {
			t.Fatalf("playlist preview argument is missing: audio only = %t", audioOnly)
		}
	}
}
