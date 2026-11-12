package mpv

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSubtitleLanguageChangesPreserveNativeCache(t *testing.T) {
	for _, sample := range []struct {
		name     string
		playlist bool
	}{{"direct", false}, {"playlist-next-and-revisit", true}} {
		t.Run(sample.name, func(t *testing.T) {
			runSubtitleCacheCheck(t, sample.playlist)
		})
	}
}

func runSubtitleCacheCheck(t *testing.T, playlist bool) {
	t.Helper()
	executable, err := exec.LookPath("mpv.exe")
	if err != nil {
		t.Skip("mpv.exe is not available on PATH")
	}
	directory := t.TempDir()
	phasePath := filepath.Join(directory, "phase.txt")
	resultPath := filepath.Join(directory, "result.json")
	if err := os.WriteFile(phasePath, []byte("load1-startup"), 0600); err != nil {
		t.Fatal(err)
	}
	wav := subtitleCacheWAV()
	var request Request
	var server *httptest.Server
	var requestMutex sync.Mutex
	type observedRequest struct {
		path  string
		phase string
	}
	var observed []observedRequest
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		phase, _ := os.ReadFile(phasePath)
		requestMutex.Lock()
		observed = append(observed, observedRequest{r.URL.Path, string(phase)})
		fixtureRequest := request
		requestMutex.Unlock()
		switch r.URL.Path {
		case "/playlist.m3u":
			w.Header().Set("Content-Type", "audio/x-mpegurl")
			_, _ = fmt.Fprintf(w, "#EXTM3U\n%s/item/1.edl\n%s/item/2.edl\n", server.URL, server.URL)
		case "/main.edl", "/item/1.edl", "/item/2.edl":
			entry := 1
			if r.URL.Path == "/item/2.edl" {
				entry = 2
			}
			item := fixtureRequest
			item.Tracks = append([]Track(nil), fixtureRequest.Tracks...)
			item.Tracks[3].URL = fmt.Sprintf("%s/%d-one.vtt", server.URL, entry)
			item.Tracks[4].URL = fmt.Sprintf("%s/%d-two.vtt", server.URL, entry)
			item.Tracks[3].Title = fmt.Sprintf("English fixture %d", entry)
			item.Tracks[4].Title = fmt.Sprintf("French fixture %d", entry)
			w.Header().Set("Content-Type", "application/x-mpv-edl")
			_, _ = w.Write([]byte(buildEDL(item)))
		case "/audio.wav":
			http.ServeContent(w, r, "audio.wav", time.Time{}, bytes.NewReader(wav))
		case "/1-one.vtt", "/1-two.vtt", "/2-one.vtt", "/2-two.vtt":
			w.Header().Set("Content-Type", "text/vtt")
			_, _ = fmt.Fprintf(w, "WEBVTT\n\n00:00:00.000 --> 00:00:29.000\n%s\n", r.URL.Path)
		default:
			http.Error(w, "unexpected media request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	mainURL := server.URL + "/main.edl"
	requestMutex.Lock()
	request = Request{
		Title:    "Native subtitle cache fixture",
		Duration: 30,
		Tracks: []Track{
			{Kind: Audio, URL: server.URL + "/audio.wav", Title: "Selected audio", SampleRate: 8000, Default: true},
			{Kind: Audio, URL: server.URL + "/unused-audio", Title: "Unused audio", MIME: `audio/mp4; codecs="mp4a.40.2"`, SampleRate: 8000},
			{Kind: Video, URL: server.URL + "/unused-video", Title: "Unused video", MIME: `video/mp4; codecs="avc1.64001f"`, Width: 320, Height: 180},
			{Kind: Subtitle, URL: server.URL + "/one.vtt", Title: "English fixture", Language: "en"},
			{Kind: Subtitle, URL: server.URL + "/two.vtt", Title: "French fixture", Language: "fr"},
		},
	}
	requestMutex.Unlock()
	loader := subtitleScript(mainURL, true)
	if playlist {
		loader = playlistScript(0, 0, false, "", "", "", server.URL+"/item/")
	}
	content := `local utils = require "mp.utils"
local phase_path = ` + luaString(phasePath) + `
local result_path = ` + luaString(resultPath) + `
local playlist = ` + fmt.Sprint(playlist) + `
local snapshots = {}
local deadline = mp.get_time() + 15
local done, loaded = false, false
local metadata_seen, loads = 0, 0
local baseline, subtitle_ids, next_change
local step = 0

local function finish(failure)
    if done then return end
    done = true
    local output = assert(io.open(result_path, "wb"))
    output:write(utils.format_json({snapshots = snapshots, failure = failure, metadata_seen = metadata_seen, loads = loads}))
    output:close()
    if failure then mp.msg.error(tostring(failure)) end
    mp.commandv("quit", failure and 1 or 0)
end

local function checked(callback)
    local success, failure = pcall(callback)
    if not success then finish(tostring(failure)) end
end

local function sample(label)
    local state = mp.get_property_native("demuxer-cache-state") or {}
    local entry = {
        label = label,
        load = loads,
        duration = mp.get_property_number("demuxer-cache-duration", -1),
        forward = tonumber(state["fw-bytes"]) or -1,
    }
    snapshots[#snapshots + 1] = entry
    if baseline then
        assert(entry.duration >= baseline.duration - 0.01, "A/V cache duration dropped after " .. label .. ": " .. entry.duration)
        assert(entry.forward >= baseline.forward, "A/V forward bytes dropped after " .. label .. ": " .. entry.forward)
    end
    return entry
end

local function set_phase(label, load)
    local phase = assert(io.open(phase_path, "wb"))
    phase:write("load" .. (load or loads) .. "-" .. label)
    phase:close()
end

local function select_subtitle(id, label)
    set_phase(label)
    mp.set_property_number("sid", id)
    sample(label .. " immediate")
    next_change = mp.get_time() + 0.25
end

mp.add_hook("on_preloaded", 49, function()
    checked(function()
        local source = mp.get_property("metadata/by-key/play-subtitles", "")
        local subtitles = utils.parse_json(source)
        assert(type(subtitles) == "table" and #subtitles == 2 and subtitles[1].source:sub(1, 6) == "edl://", "subtitle metadata is missing at on_preloaded")
        metadata_seen = metadata_seen + 1
    end)
end)
` + loader + `
mp.register_event("file-loaded", function()
    loads = loads + 1
    loaded = true
end)
mp.add_periodic_timer(0.01, function()
    if done then return end
    checked(function()
        assert(mp.get_time() < deadline, "native subtitle fixture timed out at step " .. step)
        if not loaded then return end
        if step == 0 then
            if mp.get_property_number("demuxer-cache-duration", 0) <= 20 then return end
            assert(metadata_seen == loads, "on_preloaded metadata hook did not run for this file")
            local entry = loads == 2 and 2 or 1
            if playlist then
                local path = mp.get_property("path", "")
                assert(path:sub(-11) == "/item/" .. entry .. ".edl", "playlist opened the wrong entry: " .. path)
            end
            subtitle_ids = {}
            for _, track in ipairs(mp.get_property_native("track-list") or {}) do
                if track.type == "sub" then
                    assert(track.external, "subtitle shares the A/V demuxer")
                    assert(not track.selected, "subtitle was selected before the user chose a language")
                    if #subtitle_ids == 0 then
                        assert(track.title == "English fixture " .. entry and track.lang == "en", "first native subtitle title or language does not match: " .. utils.format_json(track))
                    else
                        assert(track.title == "French fixture " .. entry and track.lang == "fr", "second native subtitle title or language does not match: " .. utils.format_json(track))
                    end
                    subtitle_ids[#subtitle_ids + 1] = track.id
                end
            end
            assert(#subtitle_ids == 2, "native external subtitle count does not equal 2")
            baseline = sample("unselected")
            assert(baseline.forward > 0, "fixture did not buffer any forward A/V bytes")
            step = 1
            select_subtitle(subtitle_ids[1], "first")
            return
        end
        sample("step " .. step)
        if mp.get_time() < next_change then return end
        local expected = step == 2 and subtitle_ids[2] or subtitle_ids[1]
        assert(mp.get_property_number("sid", -1) == expected, "native subtitle selection does not match the requested track")
        if step == 1 then
            step = 2
            select_subtitle(subtitle_ids[2], "second")
        elseif step == 2 then
            step = 3
            select_subtitle(subtitle_ids[1], "first-again")
        elseif playlist and loads < 3 then
            loaded, baseline, step = false, nil, 0
            set_phase("startup", loads + 1)
            mp.commandv(loads == 1 and "playlist-next" or "playlist-prev", "force")
        else
            finish(nil)
        end
    end)
end)
`
	scriptPath := filepath.Join(directory, "subtitle-cache.lua")
	if err := os.WriteFile(scriptPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	arguments := []string{
		"--no-config", "--load-scripts=no", "--idle=yes", "--vo=null", "--ao=null", "--input-terminal=no", "--media-controls=no",
		"--ytdl=no", "--pause", "--vid=no", "--aid=1", "--sid=auto", "--cache=yes", "--demuxer-readahead-secs=30", "--demuxer-max-bytes=4MiB",
		"--script=" + scriptPath,
	}
	if playlist {
		arguments = append(arguments, "--playlist="+server.URL+"/playlist.m3u")
	} else {
		arguments = append(arguments, mainURL)
	}
	command := exec.CommandContext(ctx, executable, arguments...)
	output, runErr := command.CombinedOutput()
	result, readErr := os.ReadFile(resultPath)
	if runErr != nil || readErr != nil {
		t.Fatalf("native subtitle cache check: execution = %v, result = %v\n%s\n%s", runErr, readErr, output, result)
	}
	var report struct {
		MetadataSeen int `json:"metadata_seen"`
		Loads        int `json:"loads"`
		Snapshots    []struct {
			Duration float64 `json:"duration"`
			Forward  int64   `json:"forward"`
		} `json:"snapshots"`
	}
	expectedLoads := 1
	if playlist {
		expectedLoads = 3
	}
	if err := json.Unmarshal(result, &report); err != nil || report.MetadataSeen != expectedLoads || report.Loads != expectedLoads || len(report.Snapshots) < expectedLoads*4 {
		t.Fatalf("native result is incomplete: error = %v\n%s", err, result)
	}
	requestMutex.Lock()
	requests := append([]observedRequest(nil), observed...)
	requestMutex.Unlock()
	expected := make(map[observedRequest]int)
	if playlist {
		expected[observedRequest{"/playlist.m3u", "load1-startup"}] = 1
	}
	for load := 1; load <= expectedLoads; load++ {
		entry := 1
		if load == 2 {
			entry = 2
		}
		mainPath := "/main.edl"
		if playlist {
			mainPath = fmt.Sprintf("/item/%d.edl", entry)
		}
		expected[observedRequest{mainPath, fmt.Sprintf("load%d-startup", load)}] = 1
		expected[observedRequest{"/audio.wav", fmt.Sprintf("load%d-startup", load)}] = 1
		expected[observedRequest{fmt.Sprintf("/%d-one.vtt", entry), fmt.Sprintf("load%d-first", load)}] = 1
		expected[observedRequest{fmt.Sprintf("/%d-two.vtt", entry), fmt.Sprintf("load%d-second", load)}] = 1
	}
	for _, actual := range requests {
		if expected[actual] == 0 {
			t.Errorf("unexpected source fetch or phase: %+v", actual)
		} else {
			expected[actual]--
		}
	}
	for actual, remaining := range expected {
		if remaining != 0 {
			t.Errorf("expected source was not fetched in its selection phase: %+v; requests = %+v", actual, requests)
		}
	}
	first := report.Snapshots[0]
	t.Logf("%d native cache samples preserved %.2fs and %d forward bytes across sid 1→2→1 for %d loads; requests = %+v", len(report.Snapshots), first.Duration, first.Forward, expectedLoads, requests)
}

func subtitleCacheWAV() []byte {
	const dataSize = 30 * 8000 * 2
	data := make([]byte, 44+dataSize)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], dataSize+36)
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 8000)
	binary.LittleEndian.PutUint32(data[28:], 16000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], dataSize)
	return data
}
