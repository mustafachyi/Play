package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"play/internal/media"
)

const nextEndpoint = "https://www.youtube.com/youtubei/v1/next?prettyPrint=false"

var descriptionChapterPattern = regexp.MustCompile(`^\s*[\[(]?((?:[0-9]+:)?[0-9]+:[0-9]{2})[\])]?\s+(.+?)\s*$`)

func (c *Client) requestChapterMetadata(ctx context.Context, videoID string) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	body, err := json.Marshal(struct {
		Context requestContext `json:"context"`
		VideoID string         `json:"videoId"`
	}{Context: requestContext{Client: browseProfile.context}, VideoID: videoID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, nextEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setPlayerHeaders(req, browseProfile, "")
	resp, err := c.http.Do(req)
	return readResponse(ctx, requestCtx, resp, err)
}

func chapterField(raw json.RawMessage, path ...string) json.RawMessage {
	for _, key := range path {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return nil
		}
		raw = fields[key]
	}
	return raw
}

func parseChapterMetadata(body []byte, videoID string, duration int64) []media.Chapter {
	var identity struct {
		CurrentVideoEndpoint struct {
			WatchEndpoint struct {
				VideoID string `json:"videoId"`
			} `json:"watchEndpoint"`
		} `json:"currentVideoEndpoint"`
	}
	if json.Unmarshal(body, &identity) != nil || identity.CurrentVideoEndpoint.WatchEndpoint.VideoID != videoID {
		return nil
	}
	bar := chapterField(body, "playerOverlays", "playerOverlayRenderer", "decoratedPlayerBarRenderer", "decoratedPlayerBarRenderer", "playerBar")
	if chapters := parseChapterRenderers(chapterField(bar, "chapteredPlayerBarRenderer", "chapters"), duration); len(chapters) > 0 {
		return chapters
	}
	var markers []struct {
		Key   string `json:"key"`
		Value struct {
			Chapters json.RawMessage `json:"chapters"`
		} `json:"value"`
	}
	_ = json.Unmarshal(chapterField(bar, "multiMarkersPlayerBarRenderer", "markersMap"), &markers)
	for _, key := range []string{"DESCRIPTION_CHAPTERS", "AUTO_CHAPTERS"} {
		for _, marker := range markers {
			if marker.Key == key {
				if chapters := parseChapterRenderers(marker.Value.Chapters, duration); len(chapters) > 0 {
					return chapters
				}
			}
		}
	}
	var panels []json.RawMessage
	_ = json.Unmarshal(chapterField(body, "engagementPanels"), &panels)
	for _, panel := range panels {
		var list struct {
			Contents []struct {
				Renderer struct {
					Title displayText `json:"title"`
					Time  displayText `json:"timeDescription"`
				} `json:"macroMarkersListItemRenderer"`
			} `json:"contents"`
		}
		if json.Unmarshal(chapterField(panel, "engagementPanelSectionListRenderer", "content", "macroMarkersListRenderer"), &list) != nil {
			continue
		}
		chapters := make([]media.Chapter, 0, len(list.Contents))
		for _, entry := range list.Contents {
			if seconds, ok := parseChapterTime(entry.Renderer.Time.String()); ok {
				chapters = append(chapters, media.Chapter{Time: seconds, Title: entry.Renderer.Title.String()})
			}
		}
		if chapters = normalizeChapters(chapters, duration); len(chapters) > 0 {
			return chapters
		}
	}
	return nil
}

func parseChapterRenderers(raw json.RawMessage, duration int64) []media.Chapter {
	var entries []struct {
		Renderer struct {
			Title displayText `json:"title"`
			Start flexibleInt `json:"timeRangeStartMillis"`
		} `json:"chapterRenderer"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	chapters := make([]media.Chapter, 0, len(entries))
	for _, entry := range entries {
		if entry.Renderer.Start.set {
			chapters = append(chapters, media.Chapter{Time: float64(entry.Renderer.Start.value) / 1000, Title: entry.Renderer.Title.String()})
		}
	}
	return normalizeChapters(chapters, duration)
}

func normalizeChapters(chapters []media.Chapter, duration int64) []media.Chapter {
	valid := chapters[:0]
	for _, chapter := range chapters {
		chapter.Title = cleanTitle(chapter.Title)
		if chapter.Title == "" || math.IsNaN(chapter.Time) || math.IsInf(chapter.Time, 0) || chapter.Time < 0 || (duration > 0 && chapter.Time >= float64(duration)) {
			continue
		}
		valid = append(valid, chapter)
	}
	sort.SliceStable(valid, func(i, j int) bool { return valid[i].Time < valid[j].Time })
	unique := valid[:0]
	for _, chapter := range valid {
		if len(unique) == 0 || unique[len(unique)-1].Time != chapter.Time {
			unique = append(unique, chapter)
		}
	}
	return unique
}

func chooseDescriptionChapters(responses []playerResponse, duration int64) []media.Chapter {
	for _, response := range responses {
		if chapters := parseDescriptionChapters(response.VideoDetails.Description, duration); len(chapters) > 0 {
			return chapters
		}
	}
	return nil
}

func parseDescriptionChapters(description string, duration int64) []media.Chapter {
	var chapters []media.Chapter
	for line := range strings.SplitSeq(description, "\n") {
		match := descriptionChapterPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		seconds, ok := parseChapterTime(match[1])
		title := cleanTitle(strings.TrimLeft(match[2], "-–—|: \t"))
		if !ok || title == "" || (duration > 0 && seconds >= float64(duration)) {
			continue
		}
		if len(chapters) > 0 && seconds-chapters[len(chapters)-1].Time < 10 {
			return nil
		}
		chapters = append(chapters, media.Chapter{Time: seconds, Title: title})
	}
	if len(chapters) < 3 || chapters[0].Time != 0 {
		return nil
	}
	return chapters
}

func parseChapterTime(value string) (float64, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var seconds float64
	for index, part := range parts {
		parsed, err := strconv.ParseUint(part, 10, 32)
		if err != nil || (index > 0 && parsed >= 60) {
			return 0, false
		}
		seconds = seconds*60 + float64(parsed)
	}
	return seconds, true
}
