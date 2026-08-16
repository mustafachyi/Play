package media

import "strings"

type Stream struct {
	URL  string
	MIME string
	Size int64
}

type Video struct {
	Stream
	Quality string
	Width   int
	Height  int
	FPS     int
	Bitrate int64
}

type Audio struct {
	Stream
	Language     string
	LanguageCode string
	Default      bool
	TrackID      string
	SampleRate   int
	Bitrate      int64
}

type Subtitle struct {
	URL          string
	Language     string
	LanguageCode string
	Auto         bool
}

type Item struct {
	Title     string
	Duration  int64
	Thumbnail string
	Videos    []Video
	Audios    []Audio
	Subtitles []Subtitle
}

type PlaylistItem struct {
	VideoID string
	Title   string
}

func DefaultVideo(videos []Video) (Video, bool) {
	var selected Video
	found := false
	for _, video := range videos {
		if video.Height <= 0 || video.Height > 1080 {
			continue
		}
		if !found || video.Height > selected.Height {
			selected = video
			found = true
		}
	}
	return selected, found
}

func DefaultAudio(audios []Audio) (Audio, bool) {
	if len(audios) == 0 {
		return Audio{}, false
	}
	for _, audio := range audios {
		if isEnglish(audio) && strings.Contains(strings.ToLower(audio.Language), "original") {
			return audio, true
		}
	}
	for _, audio := range audios {
		if isEnglish(audio) && audio.Default {
			return audio, true
		}
	}
	for _, audio := range audios {
		if isEnglish(audio) {
			return audio, true
		}
	}
	for _, audio := range audios {
		if audio.Default {
			return audio, true
		}
	}
	return audios[0], true
}

func (a Audio) Label() string {
	if strings.TrimSpace(a.Language) != "" {
		return a.Language
	}
	if strings.TrimSpace(a.LanguageCode) != "" {
		return a.LanguageCode
	}
	return "Audio"
}

func (s Subtitle) Label() string {
	label := strings.TrimSpace(s.Language)
	if label == "" {
		label = strings.TrimSpace(s.LanguageCode)
	}
	if label == "" {
		label = "Subtitles"
	}
	lower := strings.ToLower(label)
	if s.Auto && !strings.Contains(lower, "auto") && !strings.Contains(lower, "generated") {
		return label + " (auto-generated)"
	}
	return label
}

func isEnglish(audio Audio) bool {
	code := strings.ToLower(strings.TrimSpace(audio.LanguageCode))
	if code == "en" || strings.HasPrefix(code, "en-") || strings.HasPrefix(code, "en_") {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(audio.Language))
	return name == "english" || strings.HasPrefix(name, "english ") || strings.HasPrefix(name, "english (")
}
