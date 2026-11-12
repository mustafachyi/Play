package youtube

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func encodeStoryboard(t *testing.T, spec string, recommended any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"playerStoryboardSpecRenderer": map[string]any{"spec": spec, "recommendedLevel": recommended},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestStoryboardPreservesNativeTimingAndSignedPagePattern(t *testing.T) {
	spec := "https://i.ytimg.com/sb/video/storyboard3_L$L/$N.jpg?sqp=token|48#27#100#10#10#0#default#small|80#45#108#10#10#2000#M$M#medium|160#90#108#5#5#2000#M$M#a+b&c"
	got := parseStoryboard(encodeStoryboard(t, spec, 2), 213)
	if got == nil {
		t.Fatal("native storyboard was omitted")
	}
	parsed, err := url.Parse(got.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/sb/video/storyboard3_L2/M$M.jpg" || parsed.Query().Get("sigh") != "a+b&c" || parsed.Query().Get("sqp") != "token" {
		t.Fatalf("storyboard URL = %s", got.URL)
	}
	if got.Width != 160 || got.Height != 90 || got.Count != 108 || got.Columns != 5 || got.Rows != 5 || got.Interval != 2 {
		t.Fatalf("storyboard = %#v", got)
	}
}

func TestStoryboardUsesLastAvailableLevelWhenAllAreBelowPreviewWidth(t *testing.T) {
	spec := "https://i.ytimg.com/sb/video/L$L/$N.jpg|80#45#108#10#10#2000#M$M#small|160#90#108#5#5#2000#M$M#large"
	if got := parseStoryboard(encodeStoryboard(t, spec, "0"), 213); got == nil || got.Width != 160 {
		t.Fatalf("recommended storyboard = %#v", got)
	}
	for _, recommended := range []any{nil, "bad", -1, 10} {
		if got := parseStoryboard(encodeStoryboard(t, spec, recommended), 213); got == nil || got.Width != 160 {
			t.Fatalf("fallback storyboard for %v = %#v", recommended, got)
		}
	}
}

func TestStoryboardSelectsFirstRenderableLevelAtPreviewWidth(t *testing.T) {
	spec := "https://i.ytimg.com/sb/video/L$L/$N.jpg|80#45#108#10#10#2000#M$M#small|160#90#108#5#5#2000#M$M#medium|320#180#108#3#3#2000#M$M#preview|640#360#108#2#2#2000#M$M#large"
	for _, recommended := range []any{nil, 0, 1, 2, 3, "bad"} {
		got := parseStoryboard(encodeStoryboard(t, spec, recommended), 213)
		if got == nil || got.Width != 320 || got.Height != 180 || !strings.Contains(got.URL, "/L2/") {
			t.Fatalf("recommended %v selected %#v; want first level at least243 pixels wide", recommended, got)
		}
	}
}

func TestStoryboardSkipsInvalidRecommendedLevel(t *testing.T) {
	spec := "https://i.ytimg.com/sb/video/L$L/$N.jpg|80#45#108#10#10#2000#M$M#valid|0#90#108#5#5#2000#M$M#invalid"
	if got := parseStoryboard(encodeStoryboard(t, spec, 1), 213); got == nil || got.Width != 80 {
		t.Fatalf("fallback storyboard = %#v", got)
	}
}

func TestStoryboardFallsBackFromOversizedRecommendedSheet(t *testing.T) {
	spec := "https://i.ytimg.com/sb/video/L$L/$N.jpg|80#45#108#10#10#2000#M$M#small|800#800#108#5#5#2000#M$M#large"
	if got := parseStoryboard(encodeStoryboard(t, spec, 1), 213); got == nil || got.Width != 80 || got.Height != 45 {
		t.Fatalf("renderable storyboard = %#v", got)
	}
}

func TestStoryboardInfersIntervalOnlyFromKnownDuration(t *testing.T) {
	spec := "https://i.ytimg.com/sb/video/$N.jpg|48#27#100#10#10#0#default#signature"
	if got := parseStoryboard(encodeStoryboard(t, spec, 0), 250); got == nil || got.Interval != 2.5 {
		t.Fatalf("interval storyboard = %#v", got)
	}
	if got := parseStoryboard(encodeStoryboard(t, spec, 0), 0); got != nil {
		t.Fatalf("unknown timing accepted: %#v", got)
	}
}

func TestStoryboardRejectsInvalidSpecification(t *testing.T) {
	base := "https://i.ytimg.com/sb/video/L$L/$N.jpg"
	valid := "160#90#108#5#5#2000#M$M#signature"
	specs := []string{
		"",
		base,
		strings.Repeat("x", maxStoryboardSpecLength+1),
		"http://i.ytimg.com/preview.jpg|" + valid,
		"https://user@i.ytimg.com/preview.jpg|" + valid,
		base + "#fragment|" + valid,
		base + "?invalid=%ZZ|" + valid,
		base + "|160#90#108",
		base + "|0#90#108#5#5#2000#M$M#signature",
		base + "|1025#90#108#5#5#2000#M$M#signature",
		base + "|160#1025#108#5#5#2000#M$M#signature",
		base + "|160#90#1000001#5#5#2000#M$M#signature",
		base + "|160#90#108#11#10#2000#M$M#signature",
		base + "|160#90#108#0#5#2000#M$M#signature",
		base + "|160#90#108#5#5#-1#M$M#signature",
		base + "|160#90#108#5#5#NaN#M$M#signature",
		base + "|160#90#108#5#5#86400001#M$M#signature",
		base + "|160#90#108#5#5#2000#default#signature",
		base + "|160#90#108#5#5#2000##signature",
		base + "|160#90#108#5#5#2000#M$M#",
		base + "|9999999999999999999#90#108#5#5#2000#M$M#signature",
	}
	for _, spec := range specs {
		if got := parseStoryboard(encodeStoryboard(t, spec, 0), 213); got != nil {
			t.Errorf("invalid storyboard accepted: %#v", got)
		}
	}
	for _, raw := range []json.RawMessage{nil, []byte(`null`), []byte(`[]`), []byte(`{}`), []byte(`"invalid"`), []byte(`{"playerStoryboardSpecRenderer":{"spec":{}}}`)} {
		if got := parseStoryboard(raw, 213); got != nil {
			t.Errorf("invalid envelope accepted: %#v", got)
		}
	}
}

func TestOptionalStoryboardDoesNotRejectPlayableMedia(t *testing.T) {
	for _, storyboards := range []any{
		"invalid",
		[]any{},
		map[string]any{"playerStoryboardSpecRenderer": map[string]any{"spec": map[string]any{}}},
	} {
		payload := map[string]any{
			"playabilityStatus": map[string]any{"status": "OK"},
			"videoDetails":      map[string]any{"videoId": testVideoID, "title": "Video", "lengthSeconds": "213"},
			"streamingData": map[string]any{"adaptiveFormats": []any{
				map[string]any{"url": "https://video/720", "mimeType": "video/mp4", "height": 720},
				map[string]any{"url": "https://audio/en", "mimeType": "audio/mp4", "audioTrack": map[string]any{"id": "en.1", "displayName": "English"}},
			}},
			"storyboards": storyboards,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		item, err := parsePlayerResponses([][]byte{raw}, testVideoID)
		if err != nil || item.Title != "Video" || len(item.Videos) != 1 || len(item.Audios) != 1 || item.Storyboard != nil {
			t.Fatalf("playable media changed: %#v, %v", item, err)
		}
	}
}

func TestPlayerResponseExposesStoryboard(t *testing.T) {
	raw := []byte(`{
        "playabilityStatus":{"status":"OK"},
        "videoDetails":{"videoId":"dQw4w9WgXcQ","title":"Video","lengthSeconds":"213"},
        "streamingData":{"adaptiveFormats":[
            {"url":"https://video/720","mimeType":"video/mp4","height":720},
            {"url":"https://audio/en","mimeType":"audio/mp4","audioTrack":{"id":"en.1","displayName":"English"}}
        ]},
        "storyboards":{"playerStoryboardSpecRenderer":{
            "recommendedLevel":0,
            "spec":"https://i.ytimg.com/sb/video/L$L/$N.jpg|160#90#108#5#5#2000#M$M#signature"
        }}
    }`)
	item, err := parsePlayerResponses([][]byte{raw}, testVideoID)
	if err != nil || item.Storyboard == nil || item.Storyboard.Count != 108 || item.Storyboard.Interval != 2 {
		t.Fatalf("storyboard = %#v, error = %v", item.Storyboard, err)
	}
}
