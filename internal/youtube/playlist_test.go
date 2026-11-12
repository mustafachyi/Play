package youtube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestPlaylistFetchesOnePageAtATimeAndKeepsTitles(t *testing.T) {
	var calls []string
	request := func(ctx context.Context, continuation string) ([]byte, error) {
		calls = append(calls, continuation)
		if len(calls) == 1 {
			return []byte(`{"onResponseReceivedActions":[{"reloadContinuationItemsCommand":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa","title":{"simpleText":"First"}}},{"richItemRenderer":{"content":{"reelItemRenderer":{"videoId":"bbbbbbbbbbb","headline":{"simpleText":"Second"}}}}},{"continuationItemRenderer":{"continuationEndpoint":{"commandExecutorCommand":{"commands":[{"continuationCommand":{"token":"next-token"}}]}}}}]}}]}`), nil
		}
		return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"lockupViewModel":{"contentId":"ccccccccccc","contentType":"LOCKUP_CONTENT_TYPE_VIDEO","metadata":{"lockupMetadataViewModel":{"title":{"content":"Third"}}}}}]}}]}`), nil
	}
	playlist, err := newPlaylist("PL123", request)
	if err != nil {
		t.Fatal(err)
	}
	first, more, err := playlist.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !more || len(first) != 2 || first[0].Title != "First" || first[1].Title != "Second" {
		t.Fatalf("calls = %#v, more = %v, items = %#v", calls, more, first)
	}
	second, more, err := playlist.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || more || len(second) != 1 || second[0].Title != "Third" {
		t.Fatalf("calls = %#v, more = %v, items = %#v", calls, more, second)
	}
	third, more, err := playlist.Next(context.Background())
	if err != nil || more || len(third) != 0 || len(calls) != 2 {
		t.Fatalf("calls = %#v, more = %v, items = %#v, error = %v", calls, more, third, err)
	}
}

func TestPlaylistSkipsEmptyContinuationPages(t *testing.T) {
	var calls int
	playlist, err := newPlaylist("PL123", func(context.Context, string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"continuationItemRenderer":{"continuationEndpoint":{"continuationCommand":{"token":"next"}}}}]}}]}`), nil
		}
		return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa","title":{"simpleText":"One"}}}]}}]}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	items, more, err := playlist.Next(context.Background())
	if err != nil || more || len(items) != 1 || calls != 2 {
		t.Fatalf("items = %#v, more = %v, calls = %d, error = %v", items, more, calls, err)
	}
}

func TestPlaylistRejectsRepeatedContinuationWithoutRefetchingIt(t *testing.T) {
	var calls int
	playlist, err := newPlaylist("PL123", func(context.Context, string) ([]byte, error) {
		calls++
		return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa"}},{"continuationItemRenderer":{"continuationEndpoint":{"continuationCommand":{"token":"` + initialPlaylistContinuation("PL123") + `"}}}}]}}]}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = playlist.Next(context.Background())
	if err == nil || !strings.Contains(err.Error(), "repeated continuation") || calls != 1 {
		t.Fatalf("error = %v, calls = %d", err, calls)
	}
}

func TestPlaylistPaginationSafetyLimitsRemainBounded(t *testing.T) {
	playlist, err := newPlaylist("PL123", func(context.Context, string) ([]byte, error) {
		return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa"}}]}}]}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	playlist.pages = maxPlaylistPages
	if _, _, err := playlist.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "too many continuation pages") {
		t.Fatalf("page limit error = %v", err)
	}

	playlist, err = newPlaylist("PL123", func(context.Context, string) ([]byte, error) {
		return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa"}}]}}]}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	playlist.items = maxPlaylistItems
	if _, _, err := playlist.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "5,000-video limit") {
		t.Fatalf("item limit error = %v", err)
	}
}

func TestPlaylistLimitCountsPlayableItemsAndPreservesCursor(t *testing.T) {
	for _, test := range []struct {
		name    string
		videos  int
		unknown int
		prior   int
		want    int
		reject  bool
	}{
		{name: "exact limit", videos: maxPlaylistItems, want: maxPlaylistItems},
		{name: "overflow", videos: maxPlaylistItems + 1, reject: true},
		{name: "unknown rows do not count", videos: 1, unknown: maxPlaylistItems + 1, want: 1},
		{name: "one remaining", videos: 1, prior: maxPlaylistItems - 1, want: 1},
		{name: "exhausted", videos: 1, prior: maxPlaylistItems, reject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := playlistPageBody(test.videos, test.unknown)
			playlist, err := newPlaylist("PL123", func(context.Context, string) ([]byte, error) { return body, nil })
			if err != nil {
				t.Fatal(err)
			}
			playlist.items = test.prior
			continuation := playlist.continuation
			items, more, err := playlist.Next(context.Background())
			if test.reject {
				if err == nil || !strings.Contains(err.Error(), "5,000-video limit") {
					t.Fatalf("error = %v", err)
				}
				if len(items) != 0 || more || playlist.items != test.prior || playlist.pages != 0 || playlist.done || playlist.continuation != continuation || len(playlist.seen) != 0 {
					t.Fatalf("rejected page changed cursor: %#v", playlist)
				}
				return
			}
			if err != nil || more || len(items) != test.want || playlist.items != test.prior+test.want {
				t.Fatalf("items = %d, more = %v, count = %d, error = %v", len(items), more, playlist.items, err)
			}
		})
	}
}

func playlistPageBody(videos, unknown int) []byte {
	var body strings.Builder
	body.WriteString(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[`)
	for index := 0; index < unknown+videos; index++ {
		if index > 0 {
			body.WriteByte(',')
		}
		if index < unknown {
			body.WriteString(`{"unknownRenderer":{}}`)
		} else {
			body.WriteString(`{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa","title":{"simpleText":"Entry"}}}`)
		}
	}
	body.WriteString(`]}}]}`)
	return []byte(body.String())
}

func BenchmarkPlaylistPage(b *testing.B) {
	for _, videos := range []int{100, maxPlaylistItems, maxPlaylistItems * 10} {
		body := playlistPageBody(videos, 0)
		b.Run(fmt.Sprint(videos), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				playlist, err := newPlaylist("PL123", func(context.Context, string) ([]byte, error) { return body, nil })
				if err != nil {
					b.Fatal(err)
				}
				_, _, err = playlist.Next(context.Background())
				if videos <= maxPlaylistItems && err != nil || videos > maxPlaylistItems && err == nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestPlaylistRequestFailureDoesNotAdvanceCursor(t *testing.T) {
	calls := 0
	playlist, err := newPlaylist("PL123", func(context.Context, string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("temporary failure")
		}
		return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa"}}]}}]}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := playlist.Next(context.Background()); err == nil || err.Error() != "temporary failure" {
		t.Fatalf("first error = %v", err)
	}
	items, more, err := playlist.Next(context.Background())
	if err != nil || more || len(items) != 1 || calls != 2 {
		t.Fatalf("items = %#v, more = %v, calls = %d, error = %v", items, more, calls, err)
	}
}

func TestPlaylistInvalidResponsePreservesCursorForRetry(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"invalid JSON", `{"onResponseReceivedActions":`},
		{"null", `null`},
		{"empty object", `{}`},
		{"array", `[]`},
		{"string", `"invalid"`},
		{"error envelope", `{"error":{"code":503,"message":"unavailable"}}`},
		{"empty actions", `{"onResponseReceivedActions":[]}`},
		{"unknown action", `{"onResponseReceivedActions":[{"unknownCommand":{}}]}`},
		{"missing items", `{"onResponseReceivedActions":[{"reloadContinuationItemsCommand":{}}]}`},
		{"null items", `{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":null}}]}`},
		{"malformed items", `{"onResponseReceivedEndpoints":[{"appendContinuationItemsAction":{"continuationItems":{}}}]}`},
		{"missing fallback contents", `{"continuationContents":{"playlistVideoListContinuation":{}}}`},
		{"null fallback contents", `{"continuationContents":{"playlistVideoListContinuation":{"contents":null}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			playlist, err := newPlaylist("PL123", func(_ context.Context, continuation string) ([]byte, error) {
				calls = append(calls, continuation)
				switch len(calls) {
				case 1:
					return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa"}},{"continuationItemRenderer":{"continuationEndpoint":{"continuationCommand":{"token":"next-token"}}}}]}}]}`), nil
				case 2:
					return []byte(test.body), nil
				default:
					return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"bbbbbbbbbbb"}}]}}]}`), nil
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			items, more, err := playlist.Next(context.Background())
			if err != nil || !more || len(items) != 1 || items[0].VideoID != "aaaaaaaaaaa" {
				t.Fatalf("first items = %#v, more = %v, error = %v", items, more, err)
			}
			items, more, err = playlist.Next(context.Background())
			if err == nil || len(items) != 0 || more {
				t.Fatalf("invalid items = %#v, more = %v, error = %v", items, more, err)
			}
			if playlist.done || playlist.continuation != "next-token" || playlist.pages != 1 || playlist.items != 1 || len(playlist.seen) != 1 {
				t.Fatalf("cursor advanced after invalid response: %#v", playlist)
			}
			items, more, err = playlist.Next(context.Background())
			if err != nil || more || len(items) != 1 || items[0].VideoID != "bbbbbbbbbbb" || len(calls) != 3 || calls[1] != "next-token" || calls[2] != "next-token" {
				t.Fatalf("retry items = %#v, more = %v, calls = %#v, error = %v", items, more, calls, err)
			}
		})
	}
}

func TestPlaylistRecognizedEmptyResponseCompletes(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"actions reload", `{"onResponseReceivedActions":[{"reloadContinuationItemsCommand":{"continuationItems":[]}}]}`},
		{"actions append", `{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[]}}]}`},
		{"endpoints reload", `{"onResponseReceivedEndpoints":[{"reloadContinuationItemsCommand":{"continuationItems":[]}}]}`},
		{"endpoints append", `{"onResponseReceivedEndpoints":[{"appendContinuationItemsAction":{"continuationItems":[]}}]}`},
		{"fallback continuation items", `{"continuationContents":{"playlistVideoListContinuation":{"continuationItems":[]}}}`},
		{"fallback contents", `{"continuationContents":{"playlistVideoListContinuation":{"contents":[]}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			playlist, err := newPlaylist("PL123", func(context.Context, string) ([]byte, error) {
				calls++
				if calls == 1 {
					return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa"}},{"continuationItemRenderer":{"continuationEndpoint":{"continuationCommand":{"token":"next-token"}}}}]}}]}`), nil
				}
				return []byte(test.body), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			items, more, err := playlist.Next(context.Background())
			if err != nil || !more || len(items) != 1 {
				t.Fatalf("first items = %#v, more = %v, error = %v", items, more, err)
			}
			items, more, err = playlist.Next(context.Background())
			if err != nil || more || len(items) != 0 || !playlist.done || calls != 2 {
				t.Fatalf("terminal items = %#v, more = %v, calls = %d, error = %v", items, more, calls, err)
			}
			if _, _, err := playlist.Next(context.Background()); err != nil || calls != 2 {
				t.Fatalf("completed playlist refetched: calls = %d, error = %v", calls, err)
			}
		})
	}
}

func TestPlaylistNonemptyResponseTakesPrecedenceOverEmptyContainers(t *testing.T) {
	for _, body := range []string{
		`{"onResponseReceivedActions":[{"reloadContinuationItemsCommand":{"continuationItems":[]}},{"appendContinuationItemsAction":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa"}}]}}]}`,
		`{"onResponseReceivedActions":[{"reloadContinuationItemsCommand":{"continuationItems":[]}}],"onResponseReceivedEndpoints":[{"appendContinuationItemsAction":{"continuationItems":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa"}}]}}]}`,
		`{"onResponseReceivedActions":[{"reloadContinuationItemsCommand":{"continuationItems":[]}}],"continuationContents":{"playlistVideoListContinuation":{"contents":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa"}}]}}}`,
	} {
		items, err := playlistContinuationItems([]byte(body))
		if err != nil || len(items) != 1 {
			t.Fatalf("items = %#v, error = %v, body = %s", items, err, body)
		}
		item, ok := playlistItem(items[0])
		if !ok || item.VideoID != "aaaaaaaaaaa" {
			t.Fatalf("item = %#v, valid = %v, body = %s", item, ok, body)
		}
	}
}

func TestInitialPlaylistContinuationContainsPlaylistID(t *testing.T) {
	const playlistID = "PL1234567890"
	encoded, err := url.QueryUnescape(initialPlaylistContinuation(playlistID))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{[]byte("VL" + playlistID), []byte(playlistID), []byte(playlistContinuationPropertiesBase64)} {
		if !bytes.Contains(decoded, want) {
			t.Fatalf("continuation does not contain %q: %x", want, decoded)
		}
	}
}

func TestRequestBrowseUsesConfiguredProfile(t *testing.T) {
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-YouTube-Client-Name") != browseProfile.numericID || req.Header.Get("X-YouTube-Client-Version") != browseProfile.context.ClientVersion {
			t.Fatalf("headers = %#v", req.Header)
		}
		if req.Header.Get("Origin") != "https://www.youtube.com" || req.Header.Get("Referer") != "https://www.youtube.com/" {
			t.Fatalf("origin headers = %#v", req.Header)
		}
		var body browseRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Continuation != "token" || body.Context.Client != browseProfile.context {
			t.Fatalf("body = %#v", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"onResponseReceivedActions":[]}`)), Header: make(http.Header)}, nil
	})})
	if _, err := client.requestBrowse(context.Background(), "token"); err != nil {
		t.Fatal(err)
	}
}

func TestPlaylistContinuationItemsAcceptsFallback(t *testing.T) {
	body := []byte(`{"continuationContents":{"playlistVideoListContinuation":{"contents":[{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa","title":{"simpleText":"One"}}}]}}}`)
	items, err := playlistContinuationItems(body)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := playlistItem(items[0])
	if len(items) != 1 || !ok || item.VideoID != "aaaaaaaaaaa" || item.Title != "One" {
		t.Fatalf("items = %#v, item = %#v, ok = %v", items, item, ok)
	}
}

func TestCleanTitleRemovesLineBreaks(t *testing.T) {
	if got := cleanTitle(" one\r\n#EXTM3U\x00 "); got != "one  #EXTM3U" {
		t.Fatalf("title = %q", got)
	}
}

func TestPlaylistCancellationDoesNotAdvanceCursor(t *testing.T) {
	for _, cancelDuringRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelDuringRequest), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			playlist, err := newPlaylist("PL123", func(context.Context, string) ([]byte, error) {
				calls++
				cancel()
				return playlistPageBody(1, 0), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			continuation := playlist.continuation
			if !cancelDuringRequest {
				cancel()
			}
			items, more, err := playlist.Next(ctx)
			if !errors.Is(err, context.Canceled) || len(items) != 0 || more {
				t.Fatalf("items = %#v, more = %v, error = %v", items, more, err)
			}
			wantCalls := 0
			if cancelDuringRequest {
				wantCalls = 1
			}
			if calls != wantCalls || playlist.pages != 0 || playlist.items != 0 || len(playlist.seen) != 0 || playlist.continuation != continuation || playlist.done {
				t.Fatalf("cancellation changed cursor or issued requests: calls=%d, playlist=%#v", calls, playlist)
			}
		})
	}
}

func TestPlaylistCancellationStopsEmptyContinuationTraversal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	playlist, err := newPlaylist("PL123", func(context.Context, string) ([]byte, error) {
		calls++
		cancel()
		return []byte(`{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[{"continuationItemRenderer":{"continuationEndpoint":{"continuationCommand":{"token":"next"}}}}]}}]}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := playlist.Next(ctx); !errors.Is(err, context.Canceled) || calls != 1 || playlist.pages != 0 {
		t.Fatalf("calls=%d, pages=%d, error=%v", calls, playlist.pages, err)
	}
}

func TestPlaylistCycleDetectionDistinguishesLongContinuationSuffixes(t *testing.T) {
	prefix := strings.Repeat("opaque-continuation-", 8192)
	requests := 0
	playlist, err := newPlaylist("PL123", func(context.Context, string) ([]byte, error) {
		requests++
		token := prefix + strconv.Itoa(requests)
		if requests == 16 {
			token = prefix + "1"
		}
		return json.Marshal(map[string]any{
			"onResponseReceivedActions": []any{map[string]any{
				"appendContinuationItemsAction": map[string]any{
					"continuationItems": []any{
						map[string]any{"playlistVideoRenderer": map[string]any{"videoId": testVideoID}},
						map[string]any{"continuationItemRenderer": map[string]any{
							"continuationEndpoint": map[string]any{"continuationCommand": map[string]any{"token": token}},
						}},
					},
				},
			}},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for page := 1; page < 16; page++ {
		items, more, err := playlist.Next(context.Background())
		if err != nil || len(items) != 1 || !more {
			t.Fatalf("page=%d, count=%d, more=%v, error=%v", page, len(items), more, err)
		}
	}
	if _, _, err := playlist.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "repeated continuation") || requests != 16 || playlist.pages != 15 {
		t.Fatalf("requests=%d, pages=%d, error=%v", requests, playlist.pages, err)
	}
}
