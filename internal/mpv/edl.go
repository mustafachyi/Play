package mpv

import (
	"os"
	"strconv"
	"strings"
)

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
	var builder strings.Builder
	builder.WriteString("# mpv EDL v0\n")
	builder.WriteString("!global_tags,title=")
	builder.WriteString(edlEscape(request.Title))
	builder.WriteByte('\n')

	for _, track := range request.Tracks {
		builder.WriteString("!new_stream\n!no_clip\n!no_chapters\n")
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
		if request.Duration > 0 {
			builder.WriteString(",length=")
			builder.WriteString(strconv.FormatInt(request.Duration, 10))
		}
		builder.WriteByte('\n')
	}
	return builder.String()
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
