package mpv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"play/internal/media"
)

func TestChapterEDLTransportPreservesMetadataWithoutSubtitles(t *testing.T) {
	request := testRequest()
	request.Tracks = request.Tracks[:3]
	request.Chapters = []media.Chapter{{Time: 0, Title: "雪,play-chapters=%4%bad\nOpening"}, {Time: 29.759, Title: "Final \\\"title\\\""}}
	description, err := EDL(request)
	if err != nil {
		t.Fatal(err)
	}
	const tag = ",play-chapters="
	position := strings.Index(description, tag)
	if position < 0 {
		t.Fatalf("chapter metadata missing: %q", description)
	}
	value, remaining := readEDLValue(t, description[position+len(tag):])
	var got []media.Chapter
	if err := json.Unmarshal([]byte(value), &got); err != nil || !reflect.DeepEqual(got, request.Chapters) || !strings.HasPrefix(remaining, "\n!new_stream\n") {
		t.Fatalf("chapters = %#v, remaining = %q, error = %v", got, remaining, err)
	}
	if strings.Count(description, "!delay_open") != 2 || strings.Contains(description, "play-subtitles=") {
		t.Fatalf("chapter metadata anchor missing: %q", description)
	}
}

func TestChapterValidationRejectsInvalidNativeMetadata(t *testing.T) {
	for _, chapters := range [][]media.Chapter{
		{{Time: -1, Title: "Negative"}},
		{{Time: math.NaN(), Title: "NaN"}},
		{{Time: math.Inf(1), Title: "Infinite"}},
		{{Time: 0, Title: " "}},
		{{Time: 0, Title: "Nul\x00title"}},
		{{Time: 801, Title: "At end"}},
		{{Time: 10, Title: "Later"}, {Time: 0, Title: "Earlier"}},
		{{Time: 0, Title: "First"}, {Time: 0, Title: "Duplicate"}},
	} {
		request := testRequest()
		request.Chapters = chapters
		if _, err := EDL(request); err == nil {
			t.Fatalf("accepted invalid chapters = %#v", chapters)
		}
	}
}

func TestChapterScriptOwnershipAndClearing(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "prefix owner", true: "exact owner"}[exact], func(t *testing.T) {
			content := `local host = mp
local function run()
    local registered, path, source, actual
    local assignments = 0
    mp = {
        add_hook = function(name, priority, callback)
            assert(name == "on_preloaded" and priority == 50 and registered == nil)
            registered = callback
        end,
        get_property = function(name, fallback)
            assert(fallback == "")
            if name == "path" then return path end
            assert(name == "metadata/by-key/play-chapters")
            return source or fallback
        end,
        set_property_native = function(name, value)
            assert(name == "chapter-list")
            assignments, actual = assignments + 1, value
            return true
        end,
        msg = {error = function(message) error(message) end},
    }
` + chapterScript("owned.edl", exact) + `
    for _, foreign in ipairs({"foreign.edl", "", "foreign-owned.edl"}) do
        path = foreign
        registered()
        assert(assignments == 0)
    end
    path, source = "owned.edl", '[{"time":0,"title":"First"},{"time":5.25,"title":"Second"}]'
    registered()
    assert(assignments == 1 and #actual == 2 and actual[2].time == 5.25)
    source = ""
    registered()
    assert(assignments == 2 and #actual == 0)
    path, source = "owned.edl.foreign", '[{"time":0,"title":"Other"}]'
    registered()
    assert(assignments == ` + map[bool]string{true: "2", false: "3"}[exact] + `)
end
local success, failure = pcall(run)
mp = host
if not success then host.msg.error(tostring(failure)) end
host.commandv("quit", success and 0 or 1)
`
			runScriptCheck(t, content)
		})
	}
}

func TestNativeChapterSeekingAndPlaylistLifecycle(t *testing.T) {
	for _, playlist := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "playlist"}[playlist], func(t *testing.T) { runNativeChapterCheck(t, playlist) })
	}
}

func runNativeChapterCheck(t *testing.T, playlist bool) {
	t.Helper()
	executable, err := exec.LookPath("mpv.exe")
	if err != nil {
		t.Skip("mpv.exe is not available on PATH")
	}
	wav := subtitleCacheWAV()
	firstTitle := "雪,play-chapters=%4%bad\nOpening"
	entries := [][]media.Chapter{
		{{Time: 0, Title: firstTitle}, {Time: 5.25, Title: "Second"}},
		nil,
		{{Time: 0, Title: "Return"}, {Time: 12.5, Title: "Final"}},
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/playlist.m3u":
			w.Header().Set("Content-Type", "audio/x-mpegurl")
			_, _ = fmt.Fprintf(w, "#EXTM3U\n%s/item/1.edl\n%s/item/2.edl\n%s/item/3.edl\n", server.URL, server.URL, server.URL)
		case "/item/1.edl", "/item/2.edl", "/item/3.edl":
			index := int(r.URL.Path[len("/item/")] - '1')
			body, err := EDL(Request{
				Title: "Chapter fixture", Duration: 30, Chapters: entries[index],
				Tracks: []Track{{Kind: Audio, URL: server.URL + "/audio.wav", Title: "Audio", SampleRate: 8000, Default: true}},
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-mpv-edl")
			_, _ = fmt.Fprint(w, body)
		case "/audio.wav":
			http.ServeContent(w, r, "audio.wav", time.Time{}, bytes.NewReader(wav))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	owner := server.URL + "/item/1.edl"
	if playlist {
		owner = server.URL + "/item/"
	}
	content := chapterScript(owner, !playlist) + `
local playlist = ` + fmt.Sprint(playlist) + `
local loaded, pending, finished = 0, false, false
local target
local function fail(message)
    mp.msg.error(message)
    mp.commandv("quit", 1)
end
local function advance()
    pending = false
    if not playlist or loaded == 3 then
        finished = true
        mp.commandv("quit", 0)
    else
        mp.commandv("playlist-next", "force")
    end
end
mp.register_event("file-loaded", function()
    loaded = loaded + 1
    local chapters = mp.get_property_native("chapter-list", {})
    if loaded == 2 then
        if #chapters ~= 0 or mp.get_property_number("chapter", -1) ~= -1 then fail("chapter metadata was retained for an item without chapters") return end
        advance()
        return
    end
    local title = loaded == 1 and ` + luaString(firstTitle) + ` or "Return"
    target = loaded == 1 and 5.25 or 12.5
    if #chapters ~= 2 or chapters[1].title ~= title or chapters[2].time ~= target then fail("native chapter metadata does not match the expected title and timestamp") return end
    pending = true
    mp.set_property_number("chapter", 1)
end)
mp.add_periodic_timer(0.01, function()
    if pending and math.abs(mp.get_property_number("time-pos", -1) - target) < 0.02 then
        if mp.get_property_number("chapter", -1) ~= 1 then fail("native chapter selection does not match the requested chapter") return end
        advance()
    end
end)
mp.add_timeout(8, function() if not finished then fail("native chapter verification timed out") end end)
`
	path := filepath.Join(t.TempDir(), "chapter-native.lua")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"--no-config", "--load-scripts=no", "--vo=null", "--ao=null", "--pause=yes", "--input-terminal=no", "--media-controls=no", "--script=" + path}
	if playlist {
		args = append(args, "--playlist="+server.URL+"/playlist.m3u")
	} else {
		args = append(args, server.URL+"/item/1.edl")
	}
	output, err := exec.CommandContext(ctx, executable, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("native chapter verification: %v\n%s", err, output)
	}
}
