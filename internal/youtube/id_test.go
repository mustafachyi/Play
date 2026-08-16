package youtube

import (
	"strings"
	"testing"
)

func TestParseReference(t *testing.T) {
	const id = "dQw4w9WgXcQ"
	const list = "PL1234567890abcdef"

	tests := []struct {
		name  string
		input string
		want  Reference
		ok    bool
	}{
		{"id", id, Reference{VideoID: id}, true},
		{"trimmed id", "  " + id + "  ", Reference{VideoID: id}, true},
		{"watch", "https://www.youtube.com/watch?v=" + id + "&feature=share", Reference{VideoID: id}, true},
		{"watch playlist", "https://www.youtube.com/watch?v=" + id + "&list=" + list + "&index=4&t=7m42s", Reference{VideoID: id, PlaylistID: list, PlaylistIndex: 4, StartSeconds: 462}, true},
		{"playlist", "https://www.youtube.com/playlist?list=" + list, Reference{PlaylistID: list}, true},
		{"playlist index", "https://youtube.com/playlist?list=" + list + "&index=3", Reference{PlaylistID: list, PlaylistIndex: 3}, true},
		{"watch timestamp", "https://www.youtube.com/watch?v=" + id + "&t=7m42s", Reference{VideoID: id, StartSeconds: 462}, true},
		{"numeric timestamp", "https://www.youtube.com/watch?v=" + id + "&t=462", Reference{VideoID: id, StartSeconds: 462}, true},
		{"colon timestamp", "https://www.youtube.com/watch?v=" + id + "&t=1:02:03", Reference{VideoID: id, StartSeconds: 3723}, true},
		{"start", "https://youtube.com/embed/" + id + "?start=45", Reference{VideoID: id, StartSeconds: 45}, true},
		{"fragment", "https://youtu.be/" + id + "#t=1m2s", Reference{VideoID: id, StartSeconds: 62}, true},
		{"invalid timestamp ignored", "https://youtube.com/watch?v=" + id + "&t=bad", Reference{VideoID: id}, true},
		{"watch trailing slash", "https://youtube.com/watch/?v=" + id, Reference{VideoID: id}, true},
		{"music", "https://music.youtube.com/watch?v=" + id, Reference{VideoID: id}, true},
		{"short", "https://youtu.be/" + id + "?si=x", Reference{VideoID: id}, true},
		{"short playlist", "https://youtu.be/" + id + "?list=" + list, Reference{VideoID: id, PlaylistID: list}, true},
		{"shorts", "https://www.youtube.com/shorts/" + id, Reference{VideoID: id}, true},
		{"shorts playlist", "https://www.youtube.com/shorts/" + id + "?list=" + list, Reference{VideoID: id, PlaylistID: list}, true},
		{"embed", "https://www.youtube-nocookie.com/embed/" + id, Reference{VideoID: id}, true},
		{"live", "https://youtube.com/live/" + id + "?feature=share", Reference{VideoID: id}, true},
		{"legacy", "http://youtube.com/v/" + id, Reference{VideoID: id}, true},
		{"host case", "https://WWW.YOUTUBE.COM./watch?v=" + id, Reference{VideoID: id}, true},
		{"invalid short list ignored on video", "https://youtube.com/watch?v=" + id + "&list=x", Reference{VideoID: id}, true},
		{"invalid playlist list", "https://youtube.com/playlist?list=x", Reference{}, false},
		{"bad id", "dQw4w9WgXc!", Reference{}, false},
		{"lookalike host", "https://youtube.com.example.org/watch?v=" + id, Reference{}, false},
		{"unsupported scheme", "ftp://youtube.com/watch?v=" + id, Reference{}, false},
		{"userinfo", "https://user@youtube.com/watch?v=" + id, Reference{}, false},
		{"missing video", "https://youtube.com/watch", Reference{}, false},
		{"playlist missing list", "https://youtube.com/playlist", Reference{}, false},
		{"clip", "https://youtube.com/clip/Ugkxabc", Reference{}, false},
		{"bare host", "youtu.be/" + id, Reference{}, false},
		{"too long", strings.Repeat("a", maxInputLength+1), Reference{}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := Parse(test.input)
			if got != test.want || ok != test.ok {
				t.Fatalf("Parse(%q) = %#v, %v; want %#v, %v", test.input, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestParsePlaylistIndexRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "x", "999999999999999999999"} {
		if got := parsePlaylistIndex(value); got != 0 {
			t.Fatalf("parsePlaylistIndex(%q) = %d", value, got)
		}
	}
}

func TestMixPlaylistID(t *testing.T) {
	for _, value := range []string{"RDdQw4w9WgXcQ", "RDMM", "RDCLAK5uy_example"} {
		if !IsMixPlaylistID(value) {
			t.Fatalf("IsMixPlaylistID(%q) = false", value)
		}
	}
	if IsMixPlaylistID("PL1234567890") {
		t.Fatal("regular playlist detected as Mix")
	}
}

func TestParseTimeRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "-1", "1::2", "1m2", "1x", "1:99", "2m1h"} {
		if _, ok := parseTime(value); ok {
			t.Fatalf("parseTime(%q) unexpectedly succeeded", value)
		}
	}
}
