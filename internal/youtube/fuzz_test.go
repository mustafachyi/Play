package youtube

import (
	"encoding/json"
	"math"
	"testing"
)

func FuzzParseReference(f *testing.F) {
	for _, value := range []string{
		testVideoID,
		"https://youtube.com/watch?v=" + testVideoID + "&list=PL123&index=3&t=1h2m3s",
		"https://youtu.be/" + testVideoID + "#t=10",
		"https://youtube.com/playlist?list=PL123",
		"https://youtube.com.example/watch?v=" + testVideoID,
		"https://youtube.com/watch?v=%ZZ",
		"https://:443/watch?v=" + testVideoID,
	} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 64<<10 {
			t.Skip()
		}
		reference, ok := Parse(value)
		if !ok {
			return
		}
		if reference.VideoID != "" && validVideoID(reference.VideoID) == "" || reference.PlaylistID != "" && !playlistIDPattern.MatchString(reference.PlaylistID) || reference.VideoID == "" && reference.PlaylistID == "" || reference.StartSeconds < 0 || reference.PlaylistIndex < 0 {
			t.Fatalf("invalid parsed reference: %#v", reference)
		}
	})
}

func FuzzPlayerResponse(f *testing.F) {
	for _, raw := range []string{chapterPlayerFixture(), `{}`, `null`, `{"playabilityStatus":{"status":"LOGIN_REQUIRED"}}`, `{"videoDetails":{"title":null},"streamingData":[]}`} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			t.Skip()
		}
		item, err := parsePlayerResponses([][]byte{raw}, testVideoID)
		if err != nil {
			return
		}
		if item.Title == "" || len(item.Videos) == 0 || len(item.Audios) == 0 || item.Duration < 0 {
			t.Fatalf("incomplete playable item: %#v", item)
		}
		for index, video := range item.Videos {
			if !validHTTPS(video.URL) || video.Height <= 0 || index > 0 && video.Height >= item.Videos[index-1].Height {
				t.Fatalf("invalid selected video: %#v", video)
			}
		}
		for _, audio := range item.Audios {
			if !validHTTPS(audio.URL) {
				t.Fatalf("invalid selected audio: %#v", audio)
			}
		}
		for _, subtitle := range item.Subtitles {
			if webVTTURL(subtitle.URL) != subtitle.URL {
				t.Fatalf("unstable caption URL: %q", subtitle.URL)
			}
		}
	})
}

func FuzzStoryboard(f *testing.F) {
	for _, raw := range []string{`{}`, `null`, `{"playerStoryboardSpecRenderer":{"spec":"https://i.ytimg.com/sb/$L/$N.jpg|160#90#100#5#5#1000#M$M#signature"}}`, `{"playerStoryboardSpecRenderer":{"spec":"https://:443/image.jpg|160#90#20#5#5#1000#default#signature"}}`} {
		f.Add([]byte(raw), int64(120))
	}
	f.Fuzz(func(t *testing.T, raw []byte, duration int64) {
		if len(raw) > maxStoryboardSpecLength+1 {
			t.Skip()
		}
		storyboard := parseStoryboard(json.RawMessage(raw), duration)
		if storyboard == nil {
			return
		}
		if !validHTTPS(storyboard.URL) || storyboard.Width <= 0 || storyboard.Height <= 0 || storyboard.Columns <= 0 || storyboard.Rows <= 0 || storyboard.Columns*storyboard.Rows > 100 || storyboard.Width*storyboard.Height*storyboard.Columns*storyboard.Rows > maxStoryboardSheetPixels || storyboard.Count <= 0 || storyboard.Interval <= 0 || math.IsNaN(storyboard.Interval) || math.IsInf(storyboard.Interval, 0) {
			t.Fatalf("invalid storyboard: %#v", storyboard)
		}
	})
}

func FuzzChapterMetadata(f *testing.F) {
	f.Add(chapterMetadataFixture(`{"chapteredPlayerBarRenderer":{"chapters":[{"chapterRenderer":{"timeRangeStartMillis":0,"title":{"simpleText":"Opening"}}}]}}`), int64(120))
	f.Add([]byte(`null`), int64(0))
	f.Add([]byte(`{"currentVideoEndpoint":{"watchEndpoint":{"videoId":"foreign"}}}`), int64(-1))
	f.Fuzz(func(t *testing.T, raw []byte, duration int64) {
		if len(raw) > 64<<10 {
			t.Skip()
		}
		chapters := parseChapterMetadata(raw, testVideoID, duration)
		for index, chapter := range chapters {
			if chapter.Title == "" || chapter.Time < 0 || math.IsNaN(chapter.Time) || math.IsInf(chapter.Time, 0) || duration > 0 && chapter.Time >= float64(duration) || index > 0 && chapter.Time <= chapters[index-1].Time {
				t.Fatalf("invalid chapter: %#v", chapter)
			}
		}
	})
}
