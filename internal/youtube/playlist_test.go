package youtube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
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
		t.Fatalf("calls=%#v more=%v items=%#v", calls, more, first)
	}
	second, more, err := playlist.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || more || len(second) != 1 || second[0].Title != "Third" {
		t.Fatalf("calls=%#v more=%v items=%#v", calls, more, second)
	}
	third, more, err := playlist.Next(context.Background())
	if err != nil || more || len(third) != 0 || len(calls) != 2 {
		t.Fatalf("calls=%#v more=%v items=%#v err=%v", calls, more, third, err)
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
		t.Fatalf("items=%#v more=%v calls=%d err=%v", items, more, calls, err)
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
		t.Fatalf("error=%v calls=%d", err, calls)
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
	if _, _, err := playlist.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "5000-item safety limit") {
		t.Fatalf("item limit error = %v", err)
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
		t.Fatalf("items=%#v more=%v calls=%d err=%v", items, more, calls, err)
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
		t.Fatalf("items = %#v item=%#v ok=%v", items, item, ok)
	}
}

func TestCleanTitleRemovesLineBreaks(t *testing.T) {
	if got := cleanTitle(" one\r\n#EXTM3U\x00 "); got != "one  #EXTM3U" {
		t.Fatalf("title = %q", got)
	}
}
