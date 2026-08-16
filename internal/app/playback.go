package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"play/internal/media"
	"play/internal/mpv"
	"play/internal/stream"
	"play/internal/youtube"
)

type plannedTrack struct {
	name  string
	track mpv.Track
}

type playbackPlan struct {
	resources []stream.Resource
	tracks    []plannedTrack
	cover     string
}

func runVideo(ctx context.Context, reference youtube.Reference, audioOnly bool, mpvPath string, errOut io.Writer, deps dependencies) error {
	item, err := resolveVideo(ctx, reference.VideoID, deps)
	if err != nil {
		return err
	}
	plan, err := makePlaybackPlan(item, audioOnly)
	if err != nil {
		return err
	}
	local, err := deps.startGateway(plan.resources, newReporter(errOut))
	if err != nil {
		return fmt.Errorf("start local stream server: %w", err)
	}

	request, err := playbackRequest(item, reference.StartSeconds, plan, local)
	if err != nil {
		return errors.Join(err, gatewayCloseError(local))
	}
	request.Executable = mpvPath
	return errors.Join(deps.play(ctx, request), gatewayCloseError(local))
}

func playbackRequest(item media.Item, startSeconds int64, plan playbackPlan, local gateway) (mpv.Request, error) {
	request := mpv.Request{
		Title:        item.Title,
		Duration:     item.Duration,
		StartSeconds: startSeconds,
		Tracks:       make([]mpv.Track, 0, len(plan.tracks)),
	}
	for _, planned := range plan.tracks {
		track := planned.track
		track.URL = local.url(planned.name)
		if track.URL == "" {
			return mpv.Request{}, errors.New("local stream server returned an incomplete playback map")
		}
		request.Tracks = append(request.Tracks, track)
	}
	if plan.cover != "" {
		request.CoverURL = local.url(plan.cover)
		if request.CoverURL == "" {
			return mpv.Request{}, errors.New("local stream server returned an incomplete playback map")
		}
	}
	return request, nil
}

func makePlaybackPlan(item media.Item, audioOnly bool) (playbackPlan, error) {
	preferredAudio, ok := media.DefaultAudio(item.Audios)
	if !ok {
		return playbackPlan{}, errors.New("no audio stream is available")
	}
	preferredAudioIndex := audioIndex(item.Audios, preferredAudio)
	if preferredAudioIndex < 0 {
		return playbackPlan{}, errors.New("preferred audio stream is not present in the media set")
	}

	plan := playbackPlan{}
	if !audioOnly {
		preferredVideo, ok := media.DefaultVideo(item.Videos)
		if !ok {
			return playbackPlan{}, errors.New("no video quality at or below 1080p is available")
		}
		preferredVideoIndex := videoIndex(item.Videos, preferredVideo)
		if preferredVideoIndex < 0 {
			return playbackPlan{}, errors.New("preferred video stream is not present in the media set")
		}
		for i, video := range item.Videos {
			name := fmt.Sprintf("v%02d.%s", i+1, mediaExtension(video.MIME, "video"))
			plan.resources = append(plan.resources, stream.Resource{Name: name, URL: video.URL, MIME: video.MIME, Size: video.Size, Ranged: true})
			plan.tracks = append(plan.tracks, plannedTrack{name: name, track: mpv.Track{
				Kind: mpv.Video, Title: video.Quality, MIME: video.MIME, Width: video.Width, Height: video.Height,
				FPS: video.FPS, Bitrate: video.Bitrate, Default: i == preferredVideoIndex,
			}})
		}
	}

	for i, index := range preferredFirst(len(item.Audios), preferredAudioIndex) {
		audio := item.Audios[index]
		name := fmt.Sprintf("a%02d.%s", i+1, mediaExtension(audio.MIME, "audio"))
		plan.resources = append(plan.resources, stream.Resource{Name: name, URL: audio.URL, MIME: audio.MIME, Size: audio.Size, Ranged: true})
		plan.tracks = append(plan.tracks, plannedTrack{name: name, track: mpv.Track{
			Kind: mpv.Audio, Title: audio.Label(), Language: audio.LanguageCode, MIME: audio.MIME,
			SampleRate: audio.SampleRate, Bitrate: audio.Bitrate, Default: index == preferredAudioIndex,
		}})
	}

	for i, subtitle := range item.Subtitles {
		name := fmt.Sprintf("s%02d.vtt", i+1)
		plan.resources = append(plan.resources, stream.Resource{Name: name, URL: subtitle.URL, MIME: "text/vtt; charset=utf-8"})
		plan.tracks = append(plan.tracks, plannedTrack{name: name, track: mpv.Track{
			Kind: mpv.Subtitle, Title: subtitle.Label(), Language: subtitle.LanguageCode, MIME: "text/vtt", Default: i == 0,
		}})
	}

	if audioOnly && item.Thumbnail != "" {
		plan.cover = "cover" + thumbnailExtension(item.Thumbnail)
		plan.resources = append(plan.resources, stream.Resource{Name: plan.cover, URL: item.Thumbnail})
	}
	return plan, nil
}

func preferredFirst(length, preferred int) []int {
	order := make([]int, 0, length)
	if preferred >= 0 && preferred < length {
		order = append(order, preferred)
	}
	for i := 0; i < length; i++ {
		if i != preferred {
			order = append(order, i)
		}
	}
	return order
}

func mediaExtension(mimeType, kind string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	switch base {
	case "video/mp4":
		return "mp4"
	case "video/webm":
		return "webm"
	case "audio/mp4":
		return "m4a"
	case "audio/webm":
		return "webm"
	case "audio/ogg":
		return "ogg"
	}
	if kind == "audio" {
		return "audio"
	}
	return "video"
}

func thumbnailExtension(value string) string {
	u, err := url.Parse(value)
	if err != nil {
		return ".jpg"
	}
	ext := strings.ToLower(path.Ext(u.Path))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif":
		return ext
	default:
		return ".jpg"
	}
}

func videoIndex(videos []media.Video, target media.Video) int {
	for i, video := range videos {
		if video.URL == target.URL && video.Height == target.Height {
			return i
		}
	}
	return -1
}

func audioIndex(audios []media.Audio, target media.Audio) int {
	for i, audio := range audios {
		if audio.URL == target.URL && audio.TrackID == target.TrackID {
			return i
		}
	}
	return -1
}
