package youtube

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"play/internal/media"
)

const (
	maxStoryboardSpecLength  = 64 << 10
	maxStoryboardSheetPixels = 8 << 20
)

type storyboardEnvelope struct {
	Renderer *storyboardSpec `json:"playerStoryboardSpecRenderer"`
}

type storyboardSpec struct {
	Spec string `json:"spec"`
}

func chooseStoryboard(responses []playerResponse, duration int64) *media.Storyboard {
	for _, response := range responses {
		if selected := parseStoryboard(response.Storyboards, duration); selected != nil {
			return selected
		}
	}
	return nil
}

func parseStoryboard(raw json.RawMessage, duration int64) *media.Storyboard {
	var envelope storyboardEnvelope
	if len(raw) == 0 || len(raw) > maxStoryboardSpecLength || json.Unmarshal(raw, &envelope) != nil || envelope.Renderer == nil {
		return nil
	}
	parts := strings.Split(envelope.Renderer.Spec, "|")
	if len(parts) < 2 || len(parts[0]) > 8192 || !validHTTPS(parts[0]) {
		return nil
	}
	var selected *media.Storyboard
	for level, value := range parts[1:] {
		candidate := parseStoryboardLevel(parts[0], value, level, duration)
		if candidate == nil {
			continue
		}
		if candidate.Width >= 243 {
			return candidate
		}
		selected = candidate
	}
	return selected
}

func parseStoryboardLevel(base, value string, level int, duration int64) *media.Storyboard {
	fields := strings.Split(value, "#")
	if len(fields) != 8 || fields[6] == "" || fields[7] == "" || len(fields[7]) > 4096 {
		return nil
	}
	limits := [6]int{1024, 1024, 1_000_000, 100, 100, 86_400_000}
	var values [6]int
	for index := range values {
		parsed, err := strconv.ParseUint(fields[index], 10, 32)
		if err != nil || parsed > uint64(limits[index]) || parsed == 0 && index != 5 {
			return nil
		}
		values[index] = int(parsed)
	}
	if values[3]*values[4] > 100 || values[0]*values[1]*values[3]*values[4] > maxStoryboardSheetPixels {
		return nil
	}
	interval := float64(values[5]) / 1000
	if interval == 0 {
		if duration <= 0 {
			return nil
		}
		interval = float64(duration) / float64(values[2])
	}
	resolved := strings.ReplaceAll(strings.ReplaceAll(base, "$L", strconv.Itoa(level)), "$N", fields[6])
	parsed, err := url.Parse(resolved)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return nil
	}
	query.Set("sigh", fields[7])
	parsed.RawQuery = query.Encode()
	resolved = parsed.String()
	if values[2] > values[3]*values[4] && !strings.Contains(resolved, "$M") {
		return nil
	}
	return &media.Storyboard{
		URL: resolved, Width: values[0], Height: values[1], Count: values[2],
		Columns: values[3], Rows: values[4], Interval: interval,
	}
}
