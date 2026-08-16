package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

const testVideoID = "dQw4w9WgXcQ"

func TestFetchPlayerRetriesWithVisitorData(t *testing.T) {
	var calls atomic.Int32
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if req.Header.Get("X-YouTube-Client-Name") != playerProfiles[0].numericID {
			t.Fatalf("client name header = %q", req.Header.Get("X-YouTube-Client-Name"))
		}
		var body playerRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if call == 1 {
			if body.Context.Client.VisitorData != "" || req.Header.Get("X-Goog-Visitor-Id") != "" {
				t.Fatal("visitor data present on initial request")
			}
			return response(200, `{"playabilityStatus":{"status":"LOGIN_REQUIRED"},"responseContext":{"visitorData":"visitor-1"}}`), nil
		}
		if body.Context.Client.VisitorData != "visitor-1" || req.Header.Get("X-Goog-Visitor-Id") != "visitor-1" {
			t.Fatal("visitor data missing from retry")
		}
		return response(200, `{"playabilityStatus":{"status":"OK"},"streamingData":{}}`), nil
	})})

	body, err := client.fetchPlayer(context.Background(), playerProfiles[0], testVideoID)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !strings.Contains(string(body), `"OK"`) {
		t.Fatalf("calls = %d, body = %s", calls.Load(), body)
	}
}

func TestFetchPlayerKeepsInitialResponseWhenVisitorRetryFails(t *testing.T) {
	var calls atomic.Int32
	client := testClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return response(200, `{"playabilityStatus":{"status":"LOGIN_REQUIRED"},"responseContext":{"visitorData":"visitor-1"}}`), nil
		}
		return response(502, `{}`), nil
	})})

	body, err := client.fetchPlayer(context.Background(), playerProfiles[0], testVideoID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "LOGIN_REQUIRED") {
		t.Fatalf("body = %s", body)
	}
}

func TestFetchPlayerResponsesUsesSuccessfulProfile(t *testing.T) {
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-YouTube-Client-Name") == playerProfiles[0].numericID {
			return response(500, `{}`), nil
		}
		return response(200, `{"playabilityStatus":{"status":"OK"}}`), nil
	})})
	bodies, err := client.fetchPlayerResponses(context.Background(), testVideoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("responses = %d", len(bodies))
	}
}

func TestParsePlayerResponsesRejectsLiveStream(t *testing.T) {
	raw := []byte(`{"playabilityStatus":{"status":"OK","liveStreamability":{"liveStreamabilityRenderer":{}}},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"Live"},"streamingData":{"adaptiveFormats":[]}}`)
	_, err := parsePlayerResponses([][]byte{raw}, testVideoID)
	if !errors.Is(err, ErrLiveUnsupported) {
		t.Fatalf("error = %#v", err)
	}
}

func TestParsePlayerResponsesAllowsPostLiveRecording(t *testing.T) {
	raw := []byte(`{
        "playabilityStatus":{"status":"OK"},
        "videoDetails":{"videoId":"dQw4w9WgXcQ","title":"Archived live","isPostLiveDvr":true,"isLiveContent":true},
        "streamingData":{"adaptiveFormats":[
            {"url":"https://video/720","mimeType":"video/mp4","height":720,"contentLength":"100"},
            {"url":"https://audio/en","mimeType":"audio/mp4","contentLength":"50","audioTrack":{"id":"en.1","displayName":"English"}}
        ]}
    }`)
	item, err := parsePlayerResponses([][]byte{raw}, testVideoID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "Archived live" {
		t.Fatalf("item = %#v", item)
	}
}

func TestParsePlayerResponsesSelectsMetadataAndTracks(t *testing.T) {
	raw := []byte(`{
        "playabilityStatus":{"status":"OK"},
        "videoDetails":{"videoId":"dQw4w9WgXcQ","title":"Test title","lengthSeconds":"801","thumbnail":{"thumbnails":[
            {"url":"https://image/small.jpg","width":320,"height":180},
            {"url":"https://image/large.jpg","width":1280,"height":720}
        ]}},
        "streamingData":{"adaptiveFormats":[
            {"url":"https://video/2160","mimeType":"video/webm; codecs=\"vp9\"","width":3840,"height":2160,"fps":60,"bitrate":8000,"contentLength":"21600000"},
            {"url":"https://video/1080-low","mimeType":"video/webm","width":1920,"height":1080,"fps":60,"bitrate":4000,"contentLength":"10800000"},
            {"url":"https://video/1080","mimeType":"video/webm","width":"1920","height":"1080","fps":"60","bitrate":"5000","averageBitrate":4500,"contentLength":"12345678"},
            {"url":"https://video/720","mimeType":"video/mp4","width":1280,"height":720,"fps":30,"bitrate":3000,"contentLength":"7200000"},
            {"url":"https://audio/fr","mimeType":"audio/mp4","bitrate":1000,"contentLength":"111111","audioSampleRate":"44100","audioTrack":{"id":"fr.1","displayName":"French","audioIsDefault":true}},
            {"url":"https://audio/en-low","mimeType":"audio/webm","bitrate":900,"contentLength":"222222","audioTrack":{"id":"en-US.1","displayName":"English (US) - original"}},
            {"url":"https://audio/en","mimeType":"audio/webm; codecs=\"opus\"","bitrate":1100,"contentLength":"333333","audioSampleRate":"48000","audioTrack":{"id":"en-US.1","displayName":"English (US) - original"}},
            {"url":"https://audio/en2","mimeType":"audio/webm","bitrate":800,"contentLength":"444444","audioTrack":{"id":"en-GB.2","displayName":"English (UK)"}},
            {"url":"https://audio/unlabeled","mimeType":"audio/webm","bitrate":9999,"contentLength":"555555"}
        ]},
        "captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[
            {"baseUrl":"https://www.youtube.com/api/timedtext?v=x&lang=en","name":{"simpleText":"English (auto-generated)"},"vssId":"a.en","languageCode":"en","kind":"asr"},
            {"baseUrl":"https://www.youtube.com/api/timedtext?v=x&lang=es","name":{"runs":[{"text":"Spanish"}]},"vssId":".es","languageCode":"es"}
        ]}}
    }`)

	item, err := parsePlayerResponses([][]byte{raw}, testVideoID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "Test title" || item.Duration != 801 || item.Thumbnail != "https://image/large.jpg" {
		t.Fatalf("item = %#v", item)
	}
	if len(item.Videos) != 3 || item.Videos[1].URL != "https://video/1080" || item.Videos[1].Bitrate != 5000 {
		t.Fatalf("videos = %#v", item.Videos)
	}
	if len(item.Audios) != 3 {
		t.Fatalf("audios = %#v", item.Audios)
	}
	foundEnglishOriginal := false
	for _, audio := range item.Audios {
		if audio.URL == "https://audio/en" && audio.LanguageCode == "en-US" && audio.SampleRate == 48000 && audio.Size == 333333 {
			foundEnglishOriginal = true
		}
	}
	if !foundEnglishOriginal {
		t.Fatalf("audios = %#v", item.Audios)
	}
	if len(item.Subtitles) != 2 || !strings.Contains(item.Subtitles[0].URL, "fmt=vtt") {
		t.Fatalf("subtitles = %#v", item.Subtitles)
	}
}

func TestParsePlayerResponsesDeduplicatesSubtitles(t *testing.T) {
	first := []byte(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"Test"},"streamingData":{"adaptiveFormats":[{"url":"https://video/720","mimeType":"video/mp4","height":720},{"url":"https://audio/en","mimeType":"audio/mp4","audioTrack":{"id":"en.1","displayName":"English"}}]},"captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[{"baseUrl":"https://www.youtube.com/api/timedtext?x=1","name":{"simpleText":"English"},"vssId":".en","languageCode":"en"}]}}}`)
	second := []byte(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"Test"},"streamingData":{"adaptiveFormats":[]},"captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[{"baseUrl":"https://www.youtube.com/api/timedtext?x=2","name":{"simpleText":"English"},"vssId":".en","languageCode":"en"}]}}}`)
	item, err := parsePlayerResponses([][]byte{first, second}, testVideoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Subtitles) != 1 {
		t.Fatalf("subtitles = %#v", item.Subtitles)
	}
}

func TestParsePlayerResponsesUnplayable(t *testing.T) {
	raw := []byte(`{"playabilityStatus":{"status":"UNPLAYABLE","reason":"Removed"}}`)
	_, err := parsePlayerResponses([][]byte{raw}, testVideoID)
	var playerErr *PlayerError
	if !errors.As(err, &playerErr) || playerErr.Kind != Unplayable || playerErr.Status != "UNPLAYABLE" {
		t.Fatalf("error = %#v", err)
	}
}

func TestParsePlayerResponsesRejectsMismatchedVideoID(t *testing.T) {
	raw := []byte(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"aaaaaaaaaaa","title":"Wrong"},"streamingData":{"adaptiveFormats":[]}}`)
	_, err := parsePlayerResponses([][]byte{raw}, testVideoID)
	var playerErr *PlayerError
	if !errors.As(err, &playerErr) || playerErr.Kind != Protocol {
		t.Fatalf("error = %#v", err)
	}
}

func TestParsePlayerResponsesRejectsDRMOnlyVideo(t *testing.T) {
	raw := []byte(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"DRM"},"streamingData":{"adaptiveFormats":[{"url":"https://video/1080","mimeType":"video/mp4","height":1080,"drmFamilies":["WIDEVINE"]},{"url":"https://audio/en","mimeType":"audio/mp4","audioTrack":{"id":"en.1","displayName":"English"}}]}}`)
	_, err := parsePlayerResponses([][]byte{raw}, testVideoID)
	var playerErr *PlayerError
	if !errors.As(err, &playerErr) || playerErr.Kind != Protocol {
		t.Fatalf("error = %#v", err)
	}
}

func TestVisitorDataValidation(t *testing.T) {
	if got := extractVisitorData([]byte(`{"responseContext":{"visitorData":"good"}}`)); got != "good" {
		t.Fatalf("visitor data = %q", got)
	}
	if got := extractVisitorData([]byte(`{"responseContext":{"visitorData":"bad\nvalue"}}`)); got != "" {
		t.Fatalf("newline visitor data = %q", got)
	}
	if got := extractVisitorData([]byte(`not-json`)); got != "" {
		t.Fatalf("invalid visitor data = %q", got)
	}
}

func TestFlexibleIntResetsAndRejectsMalformedValues(t *testing.T) {
	var value flexibleInt
	if err := json.Unmarshal([]byte(`"42"`), &value); err != nil || !value.set || value.value != 42 {
		t.Fatalf("value = %#v err=%v", value, err)
	}
	if err := json.Unmarshal([]byte(`"bad"`), &value); err != nil || value.set || value.value != 0 {
		t.Fatalf("value after malformed input = %#v err=%v", value, err)
	}
}

func TestWebVTTURLReplacesFormatAndPreservesQuery(t *testing.T) {
	got := webVTTURL("https://www.youtube.com/api/timedtext?v=x&lang=en&fmt=srv3")
	if !strings.Contains(got, "fmt=vtt") || !strings.Contains(got, "lang=en") || strings.Contains(got, "fmt=srv3") {
		t.Fatalf("URL = %q", got)
	}
	if got := webVTTURL("http://www.youtube.com/api/timedtext?v=x"); got != "" {
		t.Fatalf("insecure URL = %q", got)
	}
}

func TestChooseVideosOrdersHeightsDescending(t *testing.T) {
	values := []format{
		{URL: "https://video/720", MIMEType: "video/mp4", Height: flexibleInt{value: 720, set: true}},
		{URL: "https://video/2160", MIMEType: "video/webm", Height: flexibleInt{value: 2160, set: true}},
		{URL: "https://video/1080", MIMEType: "video/webm", Height: flexibleInt{value: 1080, set: true}},
		{URL: "https://video/1440", MIMEType: "video/webm", Height: flexibleInt{value: 1440, set: true}},
	}
	videos := chooseVideos(values)
	want := []int{2160, 1440, 1080, 720}
	if len(videos) != len(want) {
		t.Fatalf("videos = %#v", videos)
	}
	for i, height := range want {
		if videos[i].Height != height {
			t.Fatalf("video %d height = %d; want %d", i, videos[i].Height, height)
		}
	}
}

func TestCompareBitrateUsesBitrateAverageAndStableOrder(t *testing.T) {
	if compareBitrate(2, 0, 5, 1, 100, 0) <= 0 {
		t.Fatal("primary bitrate was not preferred")
	}
	if compareBitrate(1, 2, 5, 1, 1, 0) <= 0 {
		t.Fatal("average bitrate was not preferred")
	}
	if compareBitrate(1, 1, 0, 1, 1, 5) <= 0 {
		t.Fatal("earlier order was not preferred")
	}
	if compareBitrate(1, 1, 5, 1, 1, 0) >= 0 || compareBitrate(1, 1, 0, 1, 1, 0) != 0 {
		t.Fatal("order comparison is inconsistent")
	}
}
