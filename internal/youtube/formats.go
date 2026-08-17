package youtube

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"play/internal/media"
)

func chooseVideos(values []format) []media.Video {
	selected := make(map[int]rankedVideo)
	for order, value := range values {
		candidate, ok := videoCandidate(value, order)
		if !ok {
			continue
		}
		current, exists := selected[candidate.Height]
		if !exists || compareBitrate(candidate.Bitrate, candidate.averageBitrate, candidate.order, current.Bitrate, current.averageBitrate, current.order) > 0 {
			selected[candidate.Height] = candidate
		}
	}
	ranked := make([]rankedVideo, 0, len(selected))
	for _, value := range selected {
		ranked = append(ranked, value)
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].Height > ranked[j].Height })
	videos := make([]media.Video, len(ranked))
	for i := range ranked {
		videos[i] = ranked[i].Video
	}
	return videos
}

func chooseAudios(values []format) []media.Audio {
	candidates := make([]rankedAudio, 0)
	for order, value := range values {
		candidate, ok := audioCandidate(value, order)
		if ok {
			candidates = append(candidates, candidate)
		}
	}
	hasLabels := false
	for _, candidate := range candidates {
		if candidate.group != "unlabeled" {
			hasLabels = true
			break
		}
	}
	selected := make(map[string]rankedAudio)
	for _, candidate := range candidates {
		if hasLabels && candidate.group == "unlabeled" {
			continue
		}
		current, exists := selected[candidate.group]
		if !exists || compareBitrate(candidate.Bitrate, candidate.averageBitrate, candidate.order, current.Bitrate, current.averageBitrate, current.order) > 0 {
			selected[candidate.group] = candidate
		}
	}
	ranked := make([]rankedAudio, 0, len(selected))
	for _, value := range selected {
		ranked = append(ranked, value)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Default != ranked[j].Default {
			return ranked[i].Default
		}
		left, right := strings.ToLower(ranked[i].Label()), strings.ToLower(ranked[j].Label())
		if left != right {
			return left < right
		}
		return ranked[i].order < ranked[j].order
	})
	audios := make([]media.Audio, len(ranked))
	for i := range ranked {
		audios[i] = ranked[i].Audio
	}
	return audios
}

func chooseSubtitles(responses []playerResponse) []media.Subtitle {
	selected := make(map[string]rankedSubtitle)
	order := 0
	for _, response := range responses {
		if response.Captions == nil || response.Captions.Tracklist == nil {
			continue
		}
		for _, track := range response.Captions.Tracklist.Tracks {
			captionURL := webVTTURL(track.BaseURL)
			if captionURL == "" {
				continue
			}
			name, code, id := strings.TrimSpace(track.Name.String()), strings.TrimSpace(track.LanguageCode), strings.TrimSpace(track.VSSID)
			key := id
			if key == "" {
				key = strings.ToLower(code + "|" + track.Kind + "|" + captionURL)
			}
			if _, exists := selected[key]; exists {
				continue
			}
			selected[key] = rankedSubtitle{
				Subtitle: media.Subtitle{URL: captionURL, Language: name, LanguageCode: code, Auto: track.Kind == "asr"},
				order:    order,
			}
			order++
		}
	}
	ranked := make([]rankedSubtitle, 0, len(selected))
	for _, value := range selected {
		ranked = append(ranked, value)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left, right := strings.ToLower(ranked[i].Label()), strings.ToLower(ranked[j].Label())
		if left != right {
			return left < right
		}
		if ranked[i].Auto != ranked[j].Auto {
			return !ranked[i].Auto
		}
		return ranked[i].order < ranked[j].order
	})
	subtitles := make([]media.Subtitle, len(ranked))
	for i := range ranked {
		subtitles[i] = ranked[i].Subtitle
	}
	return subtitles
}

func chooseThumbnails(responses []playerResponse, videoID string) []string {
	type candidate struct {
		url    string
		width  int64
		height int64
		known  bool
		order  int
	}

	candidates := make([]candidate, 0)
	order := 0
	addList := func(list *thumbnailList) {
		if list == nil {
			return
		}
		for _, thumbnail := range list.Thumbnails {
			if !validHTTPS(thumbnail.URL) {
				continue
			}
			candidates = append(candidates, candidate{
				url: thumbnail.URL, width: positive(thumbnail.Width), height: positive(thumbnail.Height), known: true, order: order,
			})
			order++
		}
	}
	for _, response := range responses {
		if response.VideoDetails != nil {
			addList(response.VideoDetails.Thumbnail)
		}
		if response.Microformat != nil && response.Microformat.Renderer != nil {
			addList(response.Microformat.Renderer.Thumbnail)
		}
	}

	if validVideoID(videoID) != "" {
		for _, value := range []struct {
			name   string
			width  int64
			height int64
		}{
			{name: "maxresdefault", width: 1280, height: 720},
			{name: "hq720", width: 1280, height: 720},
			{name: "sddefault", width: 640, height: 480},
			{name: "hqdefault", width: 480, height: 360},
			{name: "default", width: 120, height: 90},
		} {
			candidates = append(candidates, candidate{
				url:   "https://i.ytimg.com/vi/" + videoID + "/" + value.name + ".jpg",
				width: value.width, height: value.height, order: order,
			})
			order++
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].width != candidates[j].width {
			return candidates[i].width > candidates[j].width
		}
		if candidates[i].height != candidates[j].height {
			return candidates[i].height > candidates[j].height
		}
		if candidates[i].known != candidates[j].known {
			return candidates[i].known
		}
		return candidates[i].order < candidates[j].order
	})

	urls := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, exists := seen[candidate.url]; exists {
			continue
		}
		seen[candidate.url] = struct{}{}
		urls = append(urls, candidate.url)
	}
	return urls
}

func videoCandidate(value format, order int) (rankedVideo, bool) {
	if !strings.HasPrefix(value.MIMEType, "video/") || hasDRM(value.DRMFamilies) || value.AudioQuality != "" || value.AudioChannels.set {
		return rankedVideo{}, false
	}
	if !value.Height.set || value.Height.value <= 0 || value.Height.value > int64(^uint(0)>>1) || !validHTTPS(value.URL) {
		return rankedVideo{}, false
	}
	height := int(value.Height.value)
	bitrate := fallback(value.Bitrate, value.AverageBitrate)
	return rankedVideo{
		Video: media.Video{
			Stream:  media.Stream{URL: value.URL, MIME: value.MIMEType, Size: positive(value.ContentLength)},
			Quality: fmt.Sprintf("%dp", height),
			Width:   boundedInt(value.Width),
			Height:  height,
			FPS:     boundedInt(value.FPS),
			Bitrate: bitrate,
		},
		averageBitrate: positive(value.AverageBitrate),
		order:          order,
	}, true
}

func audioCandidate(value format, order int) (rankedAudio, bool) {
	if !strings.HasPrefix(value.MIMEType, "audio/") || hasDRM(value.DRMFamilies) || !validHTTPS(value.URL) {
		return rankedAudio{}, false
	}
	trackID, languageCode, languageName := "", "", ""
	isDefault := false
	if value.AudioTrack != nil {
		trackID = strings.TrimSpace(value.AudioTrack.ID)
		languageCode = languageCodeFromTrackID(trackID)
		languageName = strings.TrimSpace(value.AudioTrack.DisplayName)
		isDefault = value.AudioTrack.AudioIsDefault
	}
	group := "unlabeled"
	if trackID != "" {
		group = "id:" + strings.ToLower(trackID)
	} else if languageName != "" {
		group = "name:" + strings.ToLower(languageName)
	}
	bitrate := fallback(value.Bitrate, value.AverageBitrate)
	return rankedAudio{
		Audio: media.Audio{
			Stream:       media.Stream{URL: value.URL, MIME: value.MIMEType, Size: positive(value.ContentLength)},
			Language:     languageName,
			LanguageCode: languageCode,
			Default:      isDefault,
			TrackID:      trackID,
			SampleRate:   boundedInt(value.AudioSampleRate),
			Bitrate:      bitrate,
		},
		group:          group,
		averageBitrate: positive(value.AverageBitrate),
		order:          order,
	}, true
}

func compareBitrate(aBitrate, aAverage int64, aOrder int, bBitrate, bAverage int64, bOrder int) int {
	if aBitrate != bBitrate {
		if aBitrate > bBitrate {
			return 1
		}
		return -1
	}
	if aAverage != bAverage {
		if aAverage > bAverage {
			return 1
		}
		return -1
	}
	if aOrder < bOrder {
		return 1
	}
	if aOrder > bOrder {
		return -1
	}
	return 0
}

func languageCodeFromTrackID(id string) string {
	if id == "" {
		return ""
	}
	code := id
	if index := strings.IndexByte(code, '.'); index >= 0 {
		code = code[:index]
	}
	return strings.TrimSpace(code)
}

func (t displayText) String() string {
	if strings.TrimSpace(t.SimpleText) != "" {
		return t.SimpleText
	}
	var builder strings.Builder
	for _, run := range t.Runs {
		builder.WriteString(run.Text)
	}
	return builder.String()
}

func webVTTURL(value string) string {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return ""
	}
	query := u.Query()
	query.Set("fmt", "vtt")
	u.RawQuery = query.Encode()
	return u.String()
}

func validHTTPS(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

func hasDRM(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("[]"))
}

func positive(value flexibleInt) int64 {
	if value.set && value.value > 0 {
		return value.value
	}
	return 0
}

func boundedInt(value flexibleInt) int {
	if !value.set || value.value <= 0 || value.value > int64(^uint(0)>>1) {
		return 0
	}
	return int(value.value)
}

func fallback(primary, secondary flexibleInt) int64 {
	if value := positive(primary); value > 0 {
		return value
	}
	return positive(secondary)
}
