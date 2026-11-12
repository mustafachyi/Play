package mpv

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestBuildEDLSubtitlesPreserveEscapedMetadataAndOrder(t *testing.T) {
	request := testRequest()
	request.Title = "雪,play-subtitles=%3%bad\nactual title"
	wantMain, _ := splitEDLSubtitles(t, buildEDL(request))
	request.Tracks = request.Tracks[:3]
	var wantSubtitles strings.Builder
	wantSubtitles.WriteString("edl://")
	for index := 0; index < 21; index++ {
		track := Track{
			Kind:     Subtitle,
			URL:      fmt.Sprintf("http://127.0.0.1:1234/token/s%02d.vtt?x=a,b;%%25", index+1),
			Title:    fmt.Sprintf("語%d,lang=wrong;%%\nline\u2028name", index+1),
			Language: fmt.Sprintf("x-%d", index+1),
			MIME:     "text/vtt",
			Bitrate:  64000,
			Default:  index == 7,
		}
		request.Tracks = append(request.Tracks, track)
		fmt.Fprintf(&wantSubtitles, "!new_stream\n!no_clip\n!no_chapters\n!delay_open,media_type=sub,codec=webvtt\n!track_meta,title=%%%d%%%s,lang=%%%d%%%s,byterate=8000", len(track.Title), track.Title, len(track.Language), track.Language)
		if track.Default {
			wantSubtitles.WriteString(",flags=default")
		}
		fmt.Fprintf(&wantSubtitles, "\n%%%d%%%s,length=801\n", len(track.URL), track.URL)
	}

	description, err := EDL(request)
	if err != nil {
		t.Fatal(err)
	}
	main, subtitles := splitEDLSubtitles(t, description)
	if main != wantMain {
		t.Fatalf("main EDL changed after adding subtitles:\ngot %q\nwant %q", main, wantMain)
	}
	if subtitles != wantSubtitles.String() {
		t.Fatalf("subtitle EDL metadata or order changed:\ngot %q\nwant %q", subtitles, wantSubtitles.String())
	}
}

func TestBuildEDLWithoutSubtitlesOmitsSubtitleMetadata(t *testing.T) {
	request := Request{
		Title: "Audio", Duration: 10,
		Tracks: []Track{{Kind: Audio, URL: "http://127.0.0.1:1/audio", Title: "Track", MIME: `audio/webm; codecs="opus"`, Default: true}},
	}
	want := "# mpv EDL v0\n!global_tags,title=%5%Audio\n!new_stream\n!no_clip\n!no_chapters\n!delay_open,media_type=audio,codec=opus\n!track_meta,title=%5%Track,flags=default\n%24%http://127.0.0.1:1/audio,length=10\n"
	got, err := EDL(request)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("EDL without subtitles:\ngot %q\nwant %q", got, want)
	}
	main, subtitles := splitEDLSubtitles(t, got)
	if main != got || subtitles != "" {
		t.Fatalf("unexpected subtitle metadata: main = %q, subtitles = %q", main, subtitles)
	}
}

func TestBuildEDLUnknownDurationDoesNotAddSubtitleLength(t *testing.T) {
	request := testRequest()
	request.Duration = 0
	main, subtitles := splitEDLSubtitles(t, buildEDL(request))
	if strings.Contains(main, ",length=") || strings.Contains(subtitles, ",length=") {
		t.Fatalf("unknown duration added an EDL length: main = %q, subtitles = %q", main, subtitles)
	}
	if !strings.HasSuffix(subtitles, "%35%http://127.0.0.1:1234/token/s01.vtt\n") {
		t.Fatalf("subtitle URL is incomplete: %q", subtitles)
	}
}

func TestBuildEDLSubtitleOnlyRequestRetainsMainStreams(t *testing.T) {
	request := testRequest()
	request.Tracks = request.Tracks[3:]
	want := "# mpv EDL v0\n!global_tags,title=%12%Actual title\n!new_stream\n!no_clip\n!no_chapters\n!delay_open,media_type=sub,codec=webvtt\n!track_meta,title=%24%English (auto-generated),lang=%2%en,flags=default\n%35%http://127.0.0.1:1234/token/s01.vtt,length=801\n"
	got, err := EDL(request)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("subtitle-only EDL changed:\ngot %q\nwant %q", got, want)
	}
	main, subtitles := splitEDLSubtitles(t, got)
	if main != got || subtitles != "" {
		t.Fatalf("subtitle-only request was externalized: main = %q, subtitles = %q", main, subtitles)
	}
}

func TestBuildEDLSubtitleMetadataUsesSelectedMediaAnchor(t *testing.T) {
	video1 := Track{Kind: Video, URL: "http://127.0.0.1:1/V1", Title: "V1"}
	video2 := Track{Kind: Video, URL: "http://127.0.0.1:1/V2", Title: "V2", Default: true}
	audio1 := Track{Kind: Audio, URL: "http://127.0.0.1:1/A1", Title: "A1"}
	audio2 := Track{Kind: Audio, URL: "http://127.0.0.1:1/A2", Title: "A2", Default: true}
	subtitle := Track{Kind: Subtitle, URL: "http://127.0.0.1:1/S1", Title: "S1"}
	tests := []struct {
		name   string
		tracks []Track
		anchor string
	}{
		{"first audio overrides default flags", []Track{video1, video2, audio1, audio2}, "A1"},
		{"default video without audio", []Track{video1, video2}, "V2"},
		{"first video without default", []Track{video1, {Kind: Video, URL: video2.URL, Title: video2.Title}}, "V1"},
		{"audio only", []Track{audio1, audio2}, "A1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := Request{Title: "Media", Duration: 20, Tracks: append(test.tracks, subtitle)}
			description, err := EDL(request)
			if err != nil {
				t.Fatal(err)
			}
			main, subtitles := splitEDLSubtitles(t, description)
			body := strings.TrimPrefix(main, "# mpv EDL v0\n!global_tags,title=%5%Media\n")
			parts := strings.Split(body, "!new_stream\n")
			if len(parts) != len(test.tracks)+1 || parts[0] != "" {
				t.Fatalf("unexpected main EDL partitions: %q", main)
			}
			for index, track := range test.tracks {
				part := parts[index+1]
				wantPrefix := "!no_clip\n!no_chapters\n"
				if track.Title != test.anchor {
					wantPrefix += "!delay_open,media_type=" + string(track.Kind) + ",codec=null\n"
				}
				wantPrefix += "!track_meta,title=%2%" + track.Title
				if track.Default {
					wantPrefix += ",flags=default"
				}
				want := fmt.Sprintf("%s\n%%%d%%%s,length=20\n", wantPrefix, len(track.URL), track.URL)
				if part != want {
					t.Fatalf("partition %d:\ngot %q\nwant %q", index, part, want)
				}
			}
			if strings.Contains(main, subtitle.URL) || !strings.Contains(subtitles, subtitle.URL) {
				t.Fatalf("subtitle does not use its external EDL: main = %q, subtitles = %q", main, subtitles)
			}
		})
	}
}

func splitEDLSubtitles(t *testing.T, description string) (string, string) {
	t.Helper()
	const prefix = "# mpv EDL v0\n!global_tags,title="
	if !strings.HasPrefix(description, prefix) {
		t.Fatalf("EDL title metadata missing: %q", description)
	}
	_, remaining := readEDLValue(t, description[len(prefix):])
	header := description[:len(description)-len(remaining)]
	subtitles := ""
	const subtitleTag = ",play-subtitles="
	if strings.HasPrefix(remaining, subtitleTag) {
		subtitles, remaining = readEDLValue(t, remaining[len(subtitleTag):])
		var sources []subtitleSource
		if err := json.Unmarshal([]byte(subtitles), &sources); err != nil || len(sources) == 0 {
			t.Fatalf("invalid subtitle metadata: %q; error = %v", subtitles, err)
		}
		subtitles = "edl://"
		for _, source := range sources {
			if !strings.HasPrefix(source.Source, "edl://") || !strings.Contains(source.Source, "title="+edlEscape(source.Title)) || (source.Language != "" && !strings.Contains(source.Source, "lang="+edlEscape(source.Language))) {
				t.Fatalf("subtitle command metadata does not match its EDL: %+v", source)
			}
			subtitles += strings.TrimPrefix(source.Source, "edl://")
		}
	}
	if !strings.HasPrefix(remaining, "\n") {
		t.Fatalf("unexpected metadata after EDL title and subtitles: %q", remaining)
	}
	return header + remaining, subtitles
}

func readEDLValue(t *testing.T, value string) (string, string) {
	t.Helper()
	if !strings.HasPrefix(value, "%") {
		t.Fatalf("EDL value is not length-prefixed: %q", value)
	}
	end := strings.IndexByte(value[1:], '%')
	if end < 0 {
		t.Fatalf("EDL value length is incomplete: %q", value)
	}
	end++
	length, err := strconv.Atoi(value[1:end])
	if err != nil || length < 0 || length > len(value)-end-1 {
		t.Fatalf("EDL value length is invalid: %q", value)
	}
	start := end + 1
	return value[start : start+length], value[start+length:]
}
