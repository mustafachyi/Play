package app

import (
	"testing"

	"play/internal/media"
	"play/internal/stream"
)

func TestPlaylistMetadataFailureAllowsRetry(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*media.Item)
	}{
		{name: "main-title", mutate: func(item *media.Item) { item.Title = "Main\x00title" }},
		{name: "track-title", mutate: func(item *media.Item) { item.Audios[0].Language = "Track\x00title" }},
		{name: "track-language", mutate: func(item *media.Item) { item.Subtitles[0].LanguageCode = "en\x00US" }},
		{name: "chapter-title", mutate: func(item *media.Item) { item.Chapters = []media.Chapter{{Time: 0, Title: "Chapter\x00title"}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := stream.StartDynamic(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			local := gateway{url: server.URL, add: server.Add, close: server.Close}
			invalid := testItem()
			test.mutate(&invalid)
			invalidPlan, err := makePlaybackPlan(invalid, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolvePlaylistItem(invalid, invalidPlan, 1, local); err == nil {
				t.Fatal("invalid native metadata was accepted")
			}
			if value := server.URL("p0002-a01.webm"); value != "" {
				t.Errorf("failed playback plan committed resource %s", value)
			}
			valid := testItem()
			validPlan, err := makePlaybackPlan(valid, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolvePlaylistItem(valid, validPlan, 1, local); err != nil {
				t.Fatalf("healthy retry was contaminated by rejected metadata: %v", err)
			}
		})
	}
}
