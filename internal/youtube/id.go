package youtube

import (
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const maxInputLength = 2048

var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
var playlistIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{2,200}$`)
var unitTimePattern = regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

var hosts = map[string]struct{}{
	"youtube.com":              {},
	"www.youtube.com":          {},
	"m.youtube.com":            {},
	"music.youtube.com":        {},
	"youtu.be":                 {},
	"www.youtu.be":             {},
	"youtube-nocookie.com":     {},
	"www.youtube-nocookie.com": {},
}

var pathPattern = regexp.MustCompile(`^/(?:shorts|embed|live|v)/([A-Za-z0-9_-]{11})(?:/|$)`)

type Reference struct {
	VideoID       string
	PlaylistID    string
	PlaylistIndex int
	StartSeconds  int64
}

func Parse(input string) (Reference, bool) {
	value := strings.TrimSpace(input)
	if value == "" || len(value) > maxInputLength {
		return Reference{}, false
	}
	if videoIDPattern.MatchString(value) {
		return Reference{VideoID: value}, true
	}

	u, err := url.Parse(value)
	if err != nil || u.User != nil {
		return Reference{}, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Reference{}, false
	}

	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if _, ok := hosts[host]; !ok {
		return Reference{}, false
	}

	query := u.Query()
	playlistID := strings.TrimSpace(query.Get("list"))
	if !playlistIDPattern.MatchString(playlistID) {
		playlistID = ""
	}
	playlistIndex := parsePlaylistIndex(query.Get("index"))

	if strings.TrimSuffix(u.Path, "/") == "/playlist" {
		if playlistID == "" {
			return Reference{}, false
		}
		return Reference{
			PlaylistID:    playlistID,
			PlaylistIndex: playlistIndex,
			StartSeconds:  startSeconds(u),
		}, true
	}

	var candidate string
	if host == "youtu.be" || host == "www.youtu.be" {
		parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
		if len(parts) > 0 {
			candidate = parts[0]
		}
	} else if strings.TrimSuffix(u.Path, "/") == "/watch" {
		candidate = query.Get("v")
	} else if match := pathPattern.FindStringSubmatch(u.Path); len(match) == 2 {
		candidate = match[1]
	}

	if !videoIDPattern.MatchString(candidate) {
		return Reference{}, false
	}
	return Reference{
		VideoID:       candidate,
		PlaylistID:    playlistID,
		PlaylistIndex: playlistIndex,
		StartSeconds:  startSeconds(u),
	}, true
}

func IsMixPlaylistID(playlistID string) bool {
	return strings.HasPrefix(playlistID, "RD")
}

func parsePlaylistIndex(value string) int {
	index, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32)
	if err != nil || index <= 0 {
		return 0
	}
	return int(index)
}

func startSeconds(u *url.URL) int64 {
	query := u.Query()
	for _, key := range []string{"t", "start", "time_continue"} {
		if seconds, ok := parseTime(query.Get(key)); ok {
			return seconds
		}
	}
	if strings.HasPrefix(u.Fragment, "t=") {
		if seconds, ok := parseTime(strings.TrimPrefix(u.Fragment, "t=")); ok {
			return seconds
		}
	}
	return 0
}

func parseTime(value string) (int64, bool) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return seconds, seconds >= 0
	}
	if strings.Contains(value, ":") {
		parts := strings.Split(value, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return 0, false
		}
		var total int64
		for i, part := range parts {
			n, err := strconv.ParseInt(part, 10, 64)
			if err != nil || n < 0 || i > 0 && n >= 60 {
				return 0, false
			}
			if total > (math.MaxInt64-n)/60 {
				return 0, false
			}
			total = total*60 + n
		}
		return total, true
	}

	match := unitTimePattern.FindStringSubmatch(value)
	if match == nil || match[1] == "" && match[2] == "" && match[3] == "" {
		return 0, false
	}
	multipliers := []int64{3600, 60, 1}
	var total int64
	for i, part := range match[1:] {
		if part == "" {
			continue
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || n > (math.MaxInt64-total)/multipliers[i] {
			return 0, false
		}
		total += n * multipliers[i]
	}
	return total, true
}
