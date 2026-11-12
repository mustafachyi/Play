package mpv

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
)

type subtitleSource struct {
	Source   string `json:"source"`
	Title    string `json:"title"`
	Language string `json:"language"`
}

func EDL(request Request) (string, error) {
	if err := validateMediaRequest(request); err != nil {
		return "", err
	}
	return buildEDL(request), nil
}

func writeEDL(request Request) (string, error) {
	file, err := os.CreateTemp("", "play-*.edl")
	if err != nil {
		return "", err
	}
	name := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(name)
	}
	if _, err := file.WriteString(buildEDL(request)); err != nil {
		cleanup()
		return "", err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", err
	}
	return name, nil
}

func buildEDL(request Request) string {
	anchorIndex := -1
	if hasTrackKind(request.Tracks, Subtitle) || len(request.Chapters) > 0 {
		anchorIndex = metadataAnchor(request.Tracks)
	}
	var subtitles []subtitleSource
	if anchorIndex >= 0 {
		for _, track := range request.Tracks {
			if track.Kind == Subtitle {
				var source strings.Builder
				source.WriteString("edl://")
				writeEDLTrack(&source, track, request.Duration, true)
				subtitles = append(subtitles, subtitleSource{Source: source.String(), Title: track.Title, Language: track.Language})
			}
		}
	}

	var builder strings.Builder
	builder.WriteString("# mpv EDL v0\n")
	builder.WriteString("!global_tags,title=")
	builder.WriteString(edlEscape(request.Title))
	if len(subtitles) > 0 {
		builder.WriteString(",play-subtitles=")
		metadata, _ := json.Marshal(subtitles)
		builder.WriteString(edlEscape(string(metadata)))
	}
	if len(request.Chapters) > 0 {
		chapters, _ := json.Marshal(request.Chapters)
		builder.WriteString(",play-chapters=")
		builder.WriteString(edlEscape(string(chapters)))
	}
	builder.WriteByte('\n')

	for index, track := range request.Tracks {
		if track.Kind != Subtitle || anchorIndex < 0 {
			writeEDLTrack(&builder, track, request.Duration, index != anchorIndex)
		}
	}
	return builder.String()
}

func metadataAnchor(tracks []Track) int {
	for index, track := range tracks {
		if track.Kind == Audio {
			return index
		}
	}
	firstVideo := -1
	for index, track := range tracks {
		if track.Kind == Video {
			if track.Default {
				return index
			}
			if firstVideo < 0 {
				firstVideo = index
			}
		}
	}
	return firstVideo
}

func writeEDLTrack(builder *strings.Builder, track Track, duration int64, delayOpen bool) {
	builder.WriteString("!new_stream\n!no_clip\n!no_chapters\n")
	if delayOpen {
		builder.WriteString("!delay_open,media_type=")
		builder.WriteString(string(track.Kind))
		builder.WriteString(",codec=")
		builder.WriteString(codecHint(track))
		if track.Kind == Video {
			if track.Width > 0 {
				builder.WriteString(",w=")
				builder.WriteString(strconv.Itoa(track.Width))
			}
			if track.Height > 0 {
				builder.WriteString(",h=")
				builder.WriteString(strconv.Itoa(track.Height))
			}
			if track.FPS > 0 {
				builder.WriteString(",fps=")
				builder.WriteString(strconv.Itoa(track.FPS))
			}
		} else if track.Kind == Audio && track.SampleRate > 0 {
			builder.WriteString(",samplerate=")
			builder.WriteString(strconv.Itoa(track.SampleRate))
		}
		builder.WriteByte('\n')
	}

	builder.WriteString("!track_meta,title=")
	builder.WriteString(edlEscape(track.Title))
	if track.Language != "" {
		builder.WriteString(",lang=")
		builder.WriteString(edlEscape(track.Language))
	}
	if track.Bitrate > 0 {
		builder.WriteString(",byterate=")
		builder.WriteString(strconv.FormatInt(track.Bitrate/8, 10))
	}
	if track.Default {
		builder.WriteString(",flags=default")
	}
	builder.WriteByte('\n')

	builder.WriteString(edlEscape(track.URL))
	if duration > 0 {
		builder.WriteString(",length=")
		builder.WriteString(strconv.FormatInt(duration, 10))
	}
	builder.WriteByte('\n')
}

func edlEscape(value string) string {
	return "%" + strconv.Itoa(len(value)) + "%" + value
}

func codecHint(track Track) string {
	if track.Kind == Subtitle {
		return "webvtt"
	}
	mime := strings.ToLower(track.MIME)
	index := strings.Index(mime, "codecs=")
	if index < 0 {
		return "null"
	}
	value := strings.TrimSpace(mime[index+len("codecs="):])
	if strings.HasPrefix(value, `"`) {
		value = strings.TrimPrefix(value, `"`)
		if quote := strings.IndexByte(value, '"'); quote >= 0 {
			value = value[:quote]
		}
	} else if separator := strings.IndexAny(value, ",;"); separator >= 0 {
		value = value[:separator]
	}
	value = strings.TrimSpace(value)
	switch {
	case value == "vp9", strings.HasPrefix(value, "vp09"):
		return "vp9"
	case strings.HasPrefix(value, "avc1"), strings.HasPrefix(value, "avc3"):
		return "h264"
	case strings.HasPrefix(value, "av01"):
		return "av1"
	case strings.HasPrefix(value, "hev1"), strings.HasPrefix(value, "hvc1"):
		return "hevc"
	case strings.HasPrefix(value, "mp4a"):
		return "aac"
	case value == "opus":
		return "opus"
	case value == "vorbis":
		return "vorbis"
	default:
		return "null"
	}
}
