package media

import "testing"

func TestDefaultVideoDoesNotDependOnOrder(t *testing.T) {
	videos := []Video{{Quality: "360p", Height: 360}, {Quality: "2160p", Height: 2160}, {Quality: "720p", Height: 720}, {Quality: "1080p", Height: 1080}}
	got, ok := DefaultVideo(videos)
	if !ok || got.Height != 1080 {
		t.Fatalf("default video = %#v, %v", got, ok)
	}
}

func TestDefaultVideoNeverExceeds1080(t *testing.T) {
	videos := []Video{{Quality: "2160p", Height: 2160}, {Quality: "1440p", Height: 1440}}
	if _, ok := DefaultVideo(videos); ok {
		t.Fatal("expected no automatic video selection")
	}
}

func TestDefaultAudioPriority(t *testing.T) {
	audios := []Audio{
		{Language: "French", LanguageCode: "fr", Default: true},
		{Language: "English (UK)", LanguageCode: "en-GB"},
		{Language: "English (US) original", LanguageCode: "en-US"},
	}
	got, ok := DefaultAudio(audios)
	if !ok || got.LanguageCode != "en-US" {
		t.Fatalf("default audio = %#v, %v", got, ok)
	}
}

func TestDefaultAudioFallback(t *testing.T) {
	audios := []Audio{{Language: "French"}, {Language: "Spanish", Default: true}}
	got, ok := DefaultAudio(audios)
	if !ok || got.Language != "Spanish" {
		t.Fatalf("default audio = %#v, %v", got, ok)
	}
	audios = []Audio{{Language: "French"}, {Language: "German"}}
	got, ok = DefaultAudio(audios)
	if !ok || got.Language != "French" {
		t.Fatalf("fallback audio = %#v, %v", got, ok)
	}
}

func TestLabels(t *testing.T) {
	if got := (Audio{LanguageCode: "en"}).Label(); got != "en" {
		t.Fatalf("audio label = %q", got)
	}
	tests := []struct {
		subtitle Subtitle
		want     string
	}{
		{Subtitle{Language: "English", Auto: true}, "English (auto-generated)"},
		{Subtitle{Language: "English (auto-generated)", Auto: true}, "English (auto-generated)"},
		{Subtitle{Language: "Spanish"}, "Spanish"},
	}
	for _, test := range tests {
		if got := test.subtitle.Label(); got != test.want {
			t.Fatalf("Label() = %q; want %q", got, test.want)
		}
	}
}
