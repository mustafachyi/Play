package youtube

import (
	"encoding/json"
	"strconv"
	"strings"

	"play/internal/media"
)

type playerResponse struct {
	PlayabilityStatus *playabilityStatus `json:"playabilityStatus"`
	VideoDetails      *videoDetails      `json:"videoDetails"`
	StreamingData     *streamingData     `json:"streamingData"`
	Captions          *captions          `json:"captions"`
	Microformat       *playerMicroformat `json:"microformat"`
}

type playabilityStatus struct {
	Status   string   `json:"status"`
	Reason   string   `json:"reason"`
	Messages []string `json:"messages"`
}

type videoDetails struct {
	VideoID       string         `json:"videoId"`
	Title         string         `json:"title"`
	LengthSeconds flexibleInt    `json:"lengthSeconds"`
	Thumbnail     *thumbnailList `json:"thumbnail"`
	IsLive        *bool          `json:"isLive"`
}

type playerMicroformat struct {
	Renderer *playerMicroformatRenderer `json:"playerMicroformatRenderer"`
}

type playerMicroformatRenderer struct {
	Thumbnail            *thumbnailList        `json:"thumbnail"`
	LiveBroadcastDetails *liveBroadcastDetails `json:"liveBroadcastDetails"`
}

type liveBroadcastDetails struct {
	IsLiveNow bool `json:"isLiveNow"`
}

type thumbnailList struct {
	Thumbnails []thumbnail `json:"thumbnails"`
}

type thumbnail struct {
	URL    string      `json:"url"`
	Width  flexibleInt `json:"width"`
	Height flexibleInt `json:"height"`
}

type streamingData struct {
	AdaptiveFormats []format `json:"adaptiveFormats"`
}

type captions struct {
	Tracklist *captionTracklist `json:"playerCaptionsTracklistRenderer"`
}

type captionTracklist struct {
	Tracks []captionTrack `json:"captionTracks"`
}

type captionTrack struct {
	BaseURL      string      `json:"baseUrl"`
	Name         displayText `json:"name"`
	VSSID        string      `json:"vssId"`
	LanguageCode string      `json:"languageCode"`
	Kind         string      `json:"kind"`
}

type displayText struct {
	SimpleText string    `json:"simpleText"`
	Runs       []textRun `json:"runs"`
}

type textRun struct {
	Text string `json:"text"`
}

type format struct {
	URL             string          `json:"url"`
	MIMEType        string          `json:"mimeType"`
	Width           flexibleInt     `json:"width"`
	Height          flexibleInt     `json:"height"`
	FPS             flexibleInt     `json:"fps"`
	Bitrate         flexibleInt     `json:"bitrate"`
	AverageBitrate  flexibleInt     `json:"averageBitrate"`
	ContentLength   flexibleInt     `json:"contentLength"`
	AudioQuality    string          `json:"audioQuality"`
	AudioChannels   flexibleInt     `json:"audioChannels"`
	AudioSampleRate flexibleInt     `json:"audioSampleRate"`
	DRMFamilies     json.RawMessage `json:"drmFamilies"`
	AudioTrack      *audioTrack     `json:"audioTrack"`
}

type audioTrack struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	AudioIsDefault bool   `json:"audioIsDefault"`
}

type flexibleInt struct {
	value int64
	set   bool
}

func (n *flexibleInt) UnmarshalJSON(data []byte) error {
	n.value = 0
	n.set = false
	value := strings.TrimSpace(string(data))
	if value == "" || value == "null" {
		return nil
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			return nil
		}
		value = unquoted
	}
	if value == "" {
		return nil
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return nil
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil
	}
	n.value = parsed
	n.set = true
	return nil
}

type rankedVideo struct {
	media.Video
	averageBitrate int64
	order          int
}

type rankedAudio struct {
	media.Audio
	group          string
	averageBitrate int64
	order          int
}

type rankedSubtitle struct {
	media.Subtitle
	order int
}

func currentlyLive(response playerResponse) bool {
	if response.VideoDetails != nil && response.VideoDetails.IsLive != nil {
		return *response.VideoDetails.IsLive
	}
	if response.Microformat == nil || response.Microformat.Renderer == nil || response.Microformat.Renderer.LiveBroadcastDetails == nil {
		return false
	}
	return response.Microformat.Renderer.LiveBroadcastDetails.IsLiveNow
}

func parsePlayerResponses(rawResponses [][]byte, requestedVideoID string) (media.Item, error) {
	responses := make([]playerResponse, 0, len(rawResponses))
	for _, raw := range rawResponses {
		var response playerResponse
		if err := json.Unmarshal(raw, &response); err == nil {
			responses = append(responses, response)
		}
	}
	if len(responses) == 0 {
		return media.Item{}, &PlayerError{Kind: Protocol, Message: "YouTube returned no valid player response"}
	}

	for _, response := range responses {
		if response.VideoDetails != nil && response.VideoDetails.VideoID == requestedVideoID && currentlyLive(response) {
			return media.Item{}, ErrLiveUnsupported
		}
	}

	matched := make([]playerResponse, 0, len(responses))
	playable := make([]playerResponse, 0, len(responses))
	for _, response := range responses {
		if response.PlayabilityStatus == nil || response.PlayabilityStatus.Status != "OK" || response.VideoDetails == nil || response.VideoDetails.VideoID != requestedVideoID {
			continue
		}
		matched = append(matched, response)
		if response.StreamingData != nil {
			playable = append(playable, response)
		}
	}

	if len(playable) == 0 {
		for _, response := range responses {
			if response.PlayabilityStatus == nil || response.PlayabilityStatus.Status == "" || response.PlayabilityStatus.Status == "OK" {
				continue
			}
			message := strings.TrimSpace(response.PlayabilityStatus.Reason)
			if message == "" {
				for _, candidate := range response.PlayabilityStatus.Messages {
					if strings.TrimSpace(candidate) != "" {
						message = candidate
						break
					}
				}
			}
			if message == "" {
				message = "Video is not playable"
			}
			return media.Item{}, &PlayerError{Kind: Unplayable, Status: response.PlayabilityStatus.Status, Message: message}
		}
		return media.Item{}, &PlayerError{Kind: Protocol, Message: "YouTube returned no usable streaming data"}
	}

	title := ""
	var duration int64
	for _, response := range matched {
		if title == "" {
			candidate := response.VideoDetails.Title
			if strings.TrimSpace(candidate) != "" && !strings.ContainsRune(candidate, '\x00') {
				title = candidate
			}
		}
		if duration == 0 {
			duration = positive(response.VideoDetails.LengthSeconds)
		}
	}
	if title == "" {
		return media.Item{}, &PlayerError{Kind: Protocol, Message: "YouTube returned an empty video title"}
	}

	formats := make([]format, 0)
	for _, response := range playable {
		formats = append(formats, response.StreamingData.AdaptiveFormats...)
	}
	videos := chooseVideos(formats)
	if len(videos) == 0 {
		return media.Item{}, &PlayerError{Kind: Protocol, Message: "YouTube returned no usable video stream"}
	}
	audios := chooseAudios(formats)
	if len(audios) == 0 {
		return media.Item{}, &PlayerError{Kind: Protocol, Message: "YouTube returned no usable audio stream"}
	}
	return media.Item{
		Title:      title,
		Duration:   duration,
		Thumbnails: chooseThumbnails(matched, requestedVideoID),
		Videos:     videos,
		Audios:     audios,
		Subtitles:  chooseSubtitles(matched),
	}, nil
}
