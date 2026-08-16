package mpv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type TrackKind string

const (
	Video    TrackKind = "video"
	Audio    TrackKind = "audio"
	Subtitle TrackKind = "sub"
)

type Track struct {
	Kind       TrackKind
	URL        string
	Title      string
	Language   string
	MIME       string
	Width      int
	Height     int
	FPS        int
	SampleRate int
	Bitrate    int64
	Default    bool
}

type Request struct {
	Executable   string
	Title        string
	Duration     int64
	StartSeconds int64
	Tracks       []Track
	CoverURL     string
}

type PlaylistRequest struct {
	Executable   string
	URL          string
	PageURL      string
	StartIndex   int
	StartSeconds int64
	AudioOnly    bool
}

func Find() (string, error) {
	path, err := exec.LookPath("mpv.exe")
	if err != nil {
		return "", errors.New("mpv was not found in PATH")
	}
	return path, nil
}

func Run(ctx context.Context, request Request) error {
	if err := validateRequest(request); err != nil {
		return err
	}
	edlPath, err := writeEDL(request)
	if err != nil {
		return fmt.Errorf("create playback description: %w", err)
	}
	runErr := exec.CommandContext(ctx, request.Executable, arguments(request, edlPath)...).Run()
	return errors.Join(normalizeRunError(ctx, runErr), removeTemp(edlPath, "playback description"))
}

func validateRequest(request Request) error {
	if request.Executable == "" {
		return errors.New("mpv executable path is empty")
	}
	return validateMediaRequest(request)
}

func validateMediaRequest(request Request) error {
	if strings.TrimSpace(request.Title) == "" {
		return errors.New("media title is empty")
	}
	if len(request.Tracks) == 0 {
		return errors.New("no media tracks were provided")
	}
	if request.Duration < 0 || request.StartSeconds < 0 {
		return errors.New("media timing is invalid")
	}
	for _, track := range request.Tracks {
		if track.Kind != Video && track.Kind != Audio && track.Kind != Subtitle {
			return errors.New("media track type is invalid")
		}
		if !localHTTPURL(track.URL) {
			return errors.New("mpv track URLs must use the local stream server")
		}
	}
	if request.CoverURL != "" && !localHTTPURL(request.CoverURL) {
		return errors.New("mpv cover URL must use the local stream server")
	}
	return nil
}

func arguments(request Request, edlPath string) []string {
	hasVideo := hasTrackKind(request.Tracks, Video)
	audioSelection := "auto"
	if hasTrackKind(request.Tracks, Audio) {
		audioSelection = "1"
	}

	args := []string{
		"--terminal=no",
		"--ytdl=no",
		"--vid=auto",
		"--aid=" + audioSelection,
		"--sid=auto",
		"--sub-visibility=no",
		"--watch-later-options-remove=sub-pos",
		"--force-media-title=" + request.Title,
	}
	if !hasVideo {
		args = append(args, "--force-window=yes")
	}
	if command := videoCycleCommand(request.Tracks); command != "" {
		args = append(args, "--input-commands-append="+command)
	}
	if request.StartSeconds > 0 {
		args = append(args, "--start="+strconv.FormatInt(request.StartSeconds, 10))
	}
	if request.CoverURL != "" {
		args = append(args, "--cover-art-file="+request.CoverURL, "--audio-display=external-first")
	}
	return append(args, "--", edlPath)
}

func videoCycleCommand(tracks []Track) string {
	count := 0
	for _, track := range tracks {
		if track.Kind == Video {
			count++
		}
	}
	if count == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(`keybind _ "cycle-values video`)
	for id := 1; id <= count; id++ {
		builder.WriteByte(' ')
		builder.WriteString(strconv.Itoa(id))
	}
	builder.WriteByte('"')
	return builder.String()
}

func hasTrackKind(tracks []Track, kind TrackKind) bool {
	for _, track := range tracks {
		if track.Kind == kind {
			return true
		}
	}
	return false
}

func normalizeRunError(ctx context.Context, runErr error) error {
	if runErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return fmt.Errorf("mpv exited with code %d", exitErr.ExitCode())
	}
	return fmt.Errorf("start mpv: %w", runErr)
}

func removeTemp(path, kind string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("remove temporary %s: %w", kind, err)
}

func localHTTPURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return false
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
