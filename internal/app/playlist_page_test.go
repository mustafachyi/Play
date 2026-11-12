package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const playlistPageTestToken = "0123456789abcdef0123456789abcdef"

func playlistPageDirectory(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("", "play-pages-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}

func TestPlaylistPageHelperPreservesGeneratedM3U(t *testing.T) {
	var server *httptest.Server
	var body string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+playlistPageTestToken+"/page/0002.m3u" {
			t.Errorf("unexpected request path: %s", r.URL.Path)
		}
		body = "#EXTM3U\n#EXTINF:-1,العربية 日本語, punctuation\n" + server.URL + "/" + playlistPageTestToken + "/item/0002.edl\n"
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	directory := playlistPageDirectory(t)
	for range 2 {
		if err := Run(context.Background(), "test", []string{"-playlist-page", server.URL + "/" + playlistPageTestToken + "/page/0002.m3u", directory}, nil, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(filepath.Join(directory, "0002.m3u"))
	if err != nil || string(actual) != body {
		t.Fatalf("page content = %q, read error = %v; want %q", actual, err, body)
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 {
		t.Fatalf("directory entries = %v, read error = %v; want one page file", files, err)
	}
}

func TestPlaylistPageHelperRejectsInvalidRequests(t *testing.T) {
	directory := playlistPageDirectory(t)
	valid := "http://127.0.0.1:8000/" + playlistPageTestToken + "/page/0002.m3u"
	for _, value := range []string{
		"https://127.0.0.1:8000/" + playlistPageTestToken + "/page/0002.m3u",
		"http://localhost:8000/" + playlistPageTestToken + "/page/0002.m3u",
		"http://127.0.0.1:8000/not-token/page/0002.m3u",
		valid + "?query=1", valid + "?", valid + "#fragment",
		strings.Replace(valid, "0002", "0001", 1), strings.Replace(valid, "0002", "00+2", 1),
		strings.Replace(valid, "/page/", "/page/../page/", 1),
	} {
		if err := fetchPlaylistPage(context.Background(), []string{value, directory}); err == nil {
			t.Errorf("invalid playlist page URL was accepted: %q", value)
		}
	}
	for _, path := range []string{t.TempDir(), filepath.Join(directory, ".."), "relative"} {
		if err := fetchPlaylistPage(context.Background(), []string{valid, path}); err == nil {
			t.Errorf("unowned output directory was accepted: %q", path)
		}
	}
}

func TestPlaylistPageHelperRejectsInvalidResponses(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   func(string) string
		status int
	}{
		{"missing-header", func(string) string { return "not m3u\n" }, 200},
		{"local-file", func(string) string { return "#EXTM3U\n#EXTINF:-1,title\nC:/private.wav\n" }, 200},
		{"wrong-token", func(base string) string {
			return "#EXTM3U\n#EXTINF:-1,title\n" + base + "/ffffffffffffffffffffffffffffffff/item/0002.edl\n"
		}, 200},
		{"wrong-origin", func(string) string {
			return "#EXTM3U\n#EXTINF:-1,title\nhttp://127.0.0.1:1/" + playlistPageTestToken + "/item/0002.edl\n"
		}, 200},
		{"incomplete-pair", func(string) string { return "#EXTM3U\n#EXTINF:-1,title\n" }, 200},
		{"oversized", func(string) string { return strings.Repeat("x", maxPlaylistPageSize+1) }, 200},
		{"redirect", func(string) string { return "" }, 302},
		{"unavailable", func(string) string { return "" }, 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://example.com")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body(server.URL))
			}))
			defer server.Close()
			directory := playlistPageDirectory(t)
			err := fetchPlaylistPage(context.Background(), []string{server.URL + "/" + playlistPageTestToken + "/page/0002.m3u", directory})
			files, readErr := os.ReadDir(directory)
			if err == nil || readErr != nil || len(files) != 0 {
				t.Fatalf("request error = %v, directory entries = %v, read error = %v; want a rejected response and no files", err, files, readErr)
			}
		})
	}
}

func TestPlaylistPageHelperPreservesDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	directory := playlistPageDirectory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := fetchPlaylistPage(ctx, []string{server.URL + "/" + playlistPageTestToken + "/page/0002.m3u", directory})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v; want context.DeadlineExceeded", err)
	}
	files, _ := os.ReadDir(directory)
	if len(files) != 0 {
		t.Fatal("canceled request published page data")
	}
}

func TestPlaylistPageHelperRetriesAfterFailureWithoutPartialFiles(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "#EXTM3U\n")
			return
		}
		_, _ = io.WriteString(w, "#EXTM3U\n")
	}))
	defer server.Close()
	directory := playlistPageDirectory(t)
	args := []string{server.URL + "/" + playlistPageTestToken + "/page/0002.m3u", directory}
	if err := fetchPlaylistPage(context.Background(), args); err == nil {
		t.Fatal("truncated response was published")
	}
	files, _ := os.ReadDir(directory)
	if len(files) != 0 {
		t.Fatal("failed request left a partial file")
	}
	if err := fetchPlaylistPage(context.Background(), args); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(directory, "0002.m3u"))
	if err != nil || string(body) != "#EXTM3U\n" {
		t.Fatalf("terminal page = %q, read error = %v; want %q", body, err, "#EXTM3U\n")
	}
}

func BenchmarkValidatePlaylistPage(b *testing.B) {
	target, _ := playlistPageURL("http://127.0.0.1:8000/" + playlistPageTestToken + "/page/0002.m3u")
	var body strings.Builder
	body.WriteString("#EXTM3U\n")
	for id := 1; id <= 5000; id++ {
		fmt.Fprintf(&body, "#EXTINF:-1,Long title %d\nhttp://127.0.0.1:8000/%s/item/%04d.edl\n", id, playlistPageTestToken, id)
	}
	data := []byte(body.String())
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if !validPlaylistPage(data, target) {
			b.Fatal("valid page rejected")
		}
	}
}
