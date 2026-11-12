package media

import (
	"strconv"
	"testing"
)

func TestDefaultAudioPriorityCombinations(t *testing.T) {
	types := []Audio{
		{Language: "English (Original)", LanguageCode: "en-US"},
		{Language: "English", LanguageCode: "EN_gb", Default: true},
		{Language: "English", LanguageCode: "en"},
		{Language: "French", LanguageCode: "fr", Default: true},
		{Language: "German", LanguageCode: "de"},
	}
	var audios [6]Audio
	for length := 0; length <= len(audios); length++ {
		combinations := 1
		for range length {
			combinations *= len(types)
		}
		for combination := 0; combination < combinations; combination++ {
			value, preferred, priority := combination, -1, len(types)
			for index := range length {
				kind := value % len(types)
				value /= len(types)
				audios[index] = types[kind]
				audios[index].TrackID = strconv.Itoa(index)
				if kind < priority {
					preferred, priority = index, kind
				}
			}
			before := audios
			got, ok := DefaultAudio(audios[:length])
			if ok != (length > 0) || ok && got != audios[preferred] {
				t.Fatalf("length=%d combination=%d: audio=%#v selected=%v preferred=%d", length, combination, got, ok, preferred)
			}
			if audios != before {
				t.Fatalf("selection mutated tracks for length=%d combination=%d", length, combination)
			}
		}
	}
}

func TestDefaultVideoBoundaries(t *testing.T) {
	for _, test := range []struct {
		name   string
		videos []Video
		want   Video
		found  bool
	}{
		{name: "empty"},
		{name: "invalid dimensions", videos: []Video{{Height: -1}, {Height: 0}}},
		{name: "above ceiling", videos: []Video{{Height: 1081}, {Height: int(^uint(0) >> 1)}}},
		{name: "lowest positive", videos: []Video{{Height: 1}}, want: Video{Height: 1}, found: true},
		{name: "ceiling", videos: []Video{{Height: 1081}, {Height: 1080}}, want: Video{Height: 1080}, found: true},
		{name: "stable ties", videos: []Video{{Height: 720, Quality: "first"}, {Height: 720, Quality: "second"}}, want: Video{Height: 720, Quality: "first"}, found: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := DefaultVideo(test.videos)
			if got != test.want || ok != test.found {
				t.Fatalf("video=%#v selected=%v want=%#v selected=%v", got, ok, test.want, test.found)
			}
		})
	}
}
