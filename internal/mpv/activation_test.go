package mpv

import (
	"strings"
	"testing"
)

func TestPlaybackArgumentsAdvertisePlayViewLauncher(t *testing.T) {
	video := testRequest()
	videoPreview := video
	videoPreview.PreviewURL = "http://127.0.0.1:1234/token/preview.json"
	audio := Request{Title: "Audio", Tracks: []Track{{Kind: Audio}}}
	audioPreview := audio
	audioPreview.PreviewURL = videoPreview.PreviewURL
	artwork := audio
	artwork.CoverURL = "http://127.0.0.1:1234/token/cover.jpg"
	for _, test := range []struct {
		name    string
		args    []string
		preview string
	}{
		{"video", arguments(video, "video.edl"), ""},
		{"video preview", arguments(videoPreview, "video.edl"), videoPreview.PreviewURL},
		{"audio", arguments(audio, "audio.edl"), ""},
		{"audio preview", arguments(audioPreview, "audio.edl"), audioPreview.PreviewURL},
		{"artwork", arguments(artwork, "artwork.edl"), ""},
		{"video playlist", playlistArguments(PlaylistRequest{}, "playlist.lua"), "playlist"},
		{"audio playlist", playlistArguments(PlaylistRequest{AudioOnly: true}, "playlist.lua"), "playlist"},
	} {
		t.Run(test.name, func(t *testing.T) {
			markers, previews := 0, 0
			mediaTarget := false
			for _, argument := range test.args {
				if argument == "--" || strings.HasPrefix(argument, "--playlist=") {
					mediaTarget = true
				}
				if argument == "--script-opts-append=play-session=yes" {
					markers++
					if mediaTarget {
						t.Fatal("PlayView launcher marker occurs after the media target")
					}
				}
				if strings.HasPrefix(argument, "--script-opts-append=playview-preview=") {
					previews++
					if argument != "--script-opts-append=playview-preview="+test.preview {
						t.Fatalf("preview argument changed: %q", argument)
					}
				}
				if argument == "--osc" || argument == "--no-osc" || strings.HasPrefix(argument, "--osc=") || strings.HasPrefix(argument, "--script-opts=") || strings.HasPrefix(argument, "--script-opts-append=playview-mode=") {
					t.Fatalf("launcher overrides the user's controller settings: %q", argument)
				}
			}
			if markers != 1 {
				t.Fatalf("PlayView launcher marker count = %d; want 1", markers)
			}
			wantPreviews := 0
			if test.preview != "" {
				wantPreviews = 1
			}
			if previews != wantPreviews {
				t.Fatalf("preview argument count = %d; want %d", previews, wantPreviews)
			}
		})
	}
}
