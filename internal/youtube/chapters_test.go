package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"play/internal/media"
)

func chapterMetadataFixture(bar string) []byte {
	return []byte(`{"currentVideoEndpoint":{"watchEndpoint":{"videoId":"` + testVideoID + `"}},"playerOverlays":{"playerOverlayRenderer":{"decoratedPlayerBarRenderer":{"decoratedPlayerBarRenderer":{"playerBar":` + bar + `}}}}}`)
}

func TestChapterMetadataSourcesAndPrecision(t *testing.T) {
	const chapters = `[{"chapterRenderer":{"timeRangeStartMillis":0,"title":{"simpleText":"Opening"}}},{"chapterRenderer":{"timeRangeStartMillis":"29759","title":{"runs":[{"text":"雪 "},{"text":"& finale"}]}}}]`
	want := []media.Chapter{{Time: 0, Title: "Opening"}, {Time: 29.759, Title: "雪 & finale"}}
	for _, bar := range []string{
		`{"chapteredPlayerBarRenderer":{"chapters":` + chapters + `}}`,
		`{"multiMarkersPlayerBarRenderer":{"markersMap":[{"key":"AUTO_CHAPTERS","value":{"chapters":` + chapters + `}}]}}`,
		`{"multiMarkersPlayerBarRenderer":{"markersMap":[{"key":"DESCRIPTION_CHAPTERS","value":{"chapters":` + chapters + `}}]}}`,
	} {
		got := parseChapterMetadata(chapterMetadataFixture(bar), testVideoID, 60)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("chapters = %#v; want %#v", got, want)
		}
	}
}

func TestChapterMetadataPrefersCreatorMarkersAndIgnoresForeignVideo(t *testing.T) {
	bar := `{"multiMarkersPlayerBarRenderer":{"markersMap":[{"key":"AUTO_CHAPTERS","value":{"chapters":[{"chapterRenderer":{"timeRangeStartMillis":0,"title":{"simpleText":"Automatic"}}}]}},{"key":"DESCRIPTION_CHAPTERS","value":{"chapters":[{"chapterRenderer":{"timeRangeStartMillis":0,"title":{"simpleText":"Creator"}}}]}}]}}`
	body := chapterMetadataFixture(bar)
	got := parseChapterMetadata(body, testVideoID, 60)
	if len(got) != 1 || got[0].Title != "Creator" {
		t.Fatalf("chapters = %#v", got)
	}
	for _, raw := range [][]byte{[]byte(`{"error":{}}`), []byte(`not-json`), body} {
		if chapters := parseChapterMetadata(raw, "different-id", 60); len(chapters) != 0 {
			t.Fatalf("foreign chapters = %#v", chapters)
		}
	}
}

func TestChapterMetadataFiltersMalformedEntriesAndNormalizesOrder(t *testing.T) {
	bar := `{"chapteredPlayerBarRenderer":{"chapters":[
        {"chapterRenderer":{"timeRangeStartMillis":30000,"title":{"simpleText":"Final"}}},
        {"chapterRenderer":{"timeRangeStartMillis":0,"title":{"simpleText":"Opening\nline"}}},
        {"chapterRenderer":{"timeRangeStartMillis":0,"title":{"simpleText":"Duplicate"}}},
        {"chapterRenderer":{"timeRangeStartMillis":-1,"title":{"simpleText":"Negative"}}},
        {"chapterRenderer":{"title":{"simpleText":"Missing"}}},
        {"chapterRenderer":{"timeRangeStartMillis":"bad","title":{"simpleText":"Malformed"}}},
        {"chapterRenderer":{"timeRangeStartMillis":60000,"title":{"simpleText":"At end"}}},
        {"chapterRenderer":{"timeRangeStartMillis":61000,"title":{"simpleText":"After end"}}},
        {"chapterRenderer":{"timeRangeStartMillis":10000,"title":{"simpleText":" "}}}
    ]}}`
	want := []media.Chapter{{Time: 0, Title: "Opening line"}, {Time: 30, Title: "Final"}}
	if got := parseChapterMetadata(chapterMetadataFixture(bar), testVideoID, 60); !reflect.DeepEqual(got, want) {
		t.Fatalf("chapters = %#v", got)
	}
}

func TestChapterMetadataEngagementPanelFallback(t *testing.T) {
	body := []byte(`{"currentVideoEndpoint":{"watchEndpoint":{"videoId":"` + testVideoID + `"}},"engagementPanels":[{"engagementPanelSectionListRenderer":{"content":{"macroMarkersListRenderer":{"contents":[{"macroMarkersListItemRenderer":{"title":{"simpleText":"Opening"},"timeDescription":{"simpleText":"0:00"}}},{"macroMarkersListItemRenderer":{"title":{"simpleText":"Next"},"timeDescription":{"simpleText":"1:23"}}}]}}}}]}`)
	want := []media.Chapter{{Time: 0, Title: "Opening"}, {Time: 83, Title: "Next"}}
	if got := parseChapterMetadata(body, testVideoID, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("chapters = %#v", got)
	}
}

func TestDescriptionChaptersUseYouTubeChapterBoundaries(t *testing.T) {
	want := []media.Chapter{{Time: 0, Title: "Opening"}, {Time: 15, Title: "Middle"}, {Time: 3700, Title: "Final"}}
	if got := parseDescriptionChapters("Tracklist\n[00:00] Opening\n0:15 - Middle\n1:01:40 | Final\nMore text", 4000); !reflect.DeepEqual(got, want) {
		t.Fatalf("chapters = %#v", got)
	}
	for _, description := range []string{
		"0:00 Opening\n0:20 Only two",
		"0:01 Opening\n0:20 Middle\n0:40 Final",
		"0:00 Opening\n0:05 Middle\n0:40 Final",
		"0:00 Opening\n0:40 Middle\n0:20 Final",
		"0:00 Opening\n0:20 Middle\n1:00 Out of range",
		"0:00 Opening\n0:20 Middle\n0:99 Invalid",
	} {
		if got := parseDescriptionChapters(description, 60); len(got) != 0 {
			t.Fatalf("description = %q, chapters = %#v", description, got)
		}
	}
}

func chapterPlayerFixture() string {
	return `{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"` + testVideoID + `","title":"Fixture","lengthSeconds":"100","shortDescription":"0:00 Opening\n0:20 Middle\n0:40 Final"},"streamingData":{"adaptiveFormats":[{"url":"https://video/720","mimeType":"video/mp4","height":720,"contentLength":"100"},{"url":"https://audio/en","mimeType":"audio/mp4","contentLength":"50"}]}}`
}

func TestVideoChapterMetadataIsOptionalAndDoesNotRetry(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var nextCalls atomic.Int32
			client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/player") {
					return response(http.StatusOK, chapterPlayerFixture()), nil
				}
				nextCalls.Add(1)
				deadline, ok := req.Context().Deadline()
				if req.Method != http.MethodPost || req.URL.String() != nextEndpoint || !ok || time.Until(deadline) > requestTimeout || req.Header.Get("X-YouTube-Client-Name") != browseProfile.numericID {
					t.Errorf("invalid optional metadata request: %v", req)
					return nil, fmt.Errorf("invalid optional metadata request")
				}
				var body struct {
					VideoID string `json:"videoId"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.VideoID != testVideoID {
					t.Errorf("invalid metadata request body: %#v, error = %v", body, err)
					return nil, fmt.Errorf("invalid metadata request body")
				}
				return response(status, `not-json`), nil
			})})
			item, err := client.Video(context.Background(), testVideoID)
			if err != nil || len(item.Chapters) != 3 || item.Chapters[1].Time != 20 || nextCalls.Load() != 1 {
				t.Fatalf("item = %#v, calls = %d, error = %v", item, nextCalls.Load(), err)
			}
		})
	}
}

func TestVideoUsesOfficialChaptersBeforeDescription(t *testing.T) {
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/player") {
			return response(http.StatusOK, chapterPlayerFixture()), nil
		}
		return response(http.StatusOK, string(chapterMetadataFixture(`{"chapteredPlayerBarRenderer":{"chapters":[{"chapterRenderer":{"timeRangeStartMillis":0,"title":{"simpleText":"Official"}}}]}}`))), nil
	})})
	item, err := client.Video(context.Background(), testVideoID)
	if err != nil || len(item.Chapters) != 1 || item.Chapters[0].Title != "Official" {
		t.Fatalf("item = %#v, error = %v", item, err)
	}
}

func TestVideoChapterMetadataPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	metadataStarted := make(chan struct{})
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/player") {
			<-metadataStarted
			cancel()
			return response(http.StatusOK, chapterPlayerFixture()), nil
		}
		close(metadataStarted)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})})
	if _, err := client.Video(ctx, testVideoID); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestVideoRejectsOversizedOptionalChapterMetadataWithoutLosingPlayback(t *testing.T) {
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/player") {
			return response(http.StatusOK, chapterPlayerFixture()), nil
		}
		oversized := response(http.StatusOK, `{}`)
		oversized.ContentLength = maxResponseSize + 1
		return oversized, nil
	})})
	item, err := client.Video(context.Background(), testVideoID)
	if err != nil || item.Title != "Fixture" || len(item.Chapters) != 3 {
		t.Fatalf("item = %#v, error = %v", item, err)
	}
}

func TestVideoFailureCancelsOptionalChapterRequest(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/player") {
			<-started
			return response(http.StatusForbidden, `{}`), nil
		}
		close(started)
		<-req.Context().Done()
		close(stopped)
		return nil, req.Context().Err()
	})})
	if _, err := client.Video(context.Background(), testVideoID); err == nil {
		t.Fatal("player failure was ignored")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("optional chapter request survived player failure")
	}
}
