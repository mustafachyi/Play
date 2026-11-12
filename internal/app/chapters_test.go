package app

import (
	"reflect"
	"strings"
	"testing"

	"play/internal/media"
	"play/internal/stream"
)

func TestChaptersReachVideoAudioAndPlaylistRequests(t *testing.T) {
	for _, audioOnly := range []bool{false, true} {
		item := testItem()
		item.Chapters = []media.Chapter{{Time: 0, Title: "Opening"}, {Time: 15.25, Title: "Final"}}
		plan, err := makePlaybackPlan(item, audioOnly)
		if err != nil {
			t.Fatal(err)
		}
		local := gateway{
			url: func(name string) string { return "http://127.0.0.1:1234/token/" + name },
			add: func(_ []stream.Resource) error { return nil },
		}
		request, err := playbackRequest(item, 12, plan, local)
		if err != nil || !reflect.DeepEqual(request.Chapters, item.Chapters) || request.StartSeconds != 12 {
			t.Fatalf("audio only = %v, request = %#v, error = %v", audioOnly, request, err)
		}
		entry, err := resolvePlaylistItem(item, plan, 3, local)
		if err != nil || !strings.Contains(entry.EDL, ",play-chapters=") || !strings.Contains(entry.EDL, "15.25") {
			t.Fatalf("audio only = %v, entry = %#v, error = %v", audioOnly, entry, err)
		}
	}
}
