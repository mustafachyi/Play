package stream

import (
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
	"sync/atomic"
	"testing"
	"time"
)

func TestNativePlaybackRecoversFromStalledMediaRange(t *testing.T) {
	executable, err := exec.LookPath("mpv.exe")
	if err != nil {
		t.Skip("mpv.exe is not available on PATH")
	}
	const prefixSize = 32768
	payload := playbackWAV()
	var calls atomic.Int32
	var mutex sync.Mutex
	var requestedRanges []byteRange
	stalledCanceled := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		span, err := parseRange(r.Header.Get("Range"), int64(len(payload)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
			return
		}
		mutex.Lock()
		requestedRanges = append(requestedRanges, span)
		mutex.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", span.start, span.end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(span.end-span.start+1))
		w.WriteHeader(http.StatusPartialContent)
		if calls.Add(1) == 1 {
			if span.start != 0 || span.end < prefixSize {
				t.Errorf("initial native range = %+v", span)
				return
			}
			_, _ = w.Write(payload[:prefixSize])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(stalledCanceled)
			return
		}
		_, _ = w.Write(payload[span.start : span.end+1])
	}))
	defer upstream.Close()
	gateway, err := StartDynamic(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	gateway.client.CloseIdleConnections()
	gateway.client = upstream.Client()
	if err := gateway.Add([]Resource{{Name: "audio.wav", URL: upstream.URL, MIME: "audio/wav", Size: int64(len(payload)), Ranged: true}}); err != nil {
		t.Fatal(err)
	}
	gateway.handlers["audio.wav"].(*sourceHandler).readTimeout = 250 * time.Millisecond
	directory := t.TempDir()
	resultPath := filepath.Join(directory, "playback.json")
	quotedResultPath, err := json.Marshal(filepath.ToSlash(resultPath))
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`local mp = require "mp"
local utils = require "mp.utils"
local finished = false
local samples = 0
local function finish(error)
    if finished then return end
    finished = true
    local output = assert(io.open(%s, "wb"))
    output:write(utils.format_json({error = error, samples = samples, position = mp.get_property_number("time-pos", -1), cache = mp.get_property_native("demuxer-cache-state"), version = mp.get_property("mpv-version")}))
    output:close()
    mp.commandv("quit", error and 1 or 0)
end
mp.add_periodic_timer(0.05, function()
    if finished then return end
    samples = samples + 1
    if mp.get_property_number("time-pos", -1) >= 3 then finish(nil) end
end)
mp.add_timeout(6, function() finish("Playback did not advance beyond the stalled prefix") end)
`, quotedResultPath)
	scriptPath := filepath.Join(directory, "playback.lua")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "--no-config", "--load-scripts=no", "--vo=null", "--ao=null", "--input-terminal=no", "--media-controls=no", "--ytdl=no", "--script="+scriptPath, gateway.URL("audio.wav"))
	output, runErr := command.CombinedOutput()
	result, readErr := os.ReadFile(resultPath)
	if runErr != nil || readErr != nil {
		t.Fatalf("native stalled playback: execution = %v, result = %v\n%s\n%s", runErr, readErr, output, result)
	}
	var report struct {
		Error    string  `json:"error"`
		Samples  int     `json:"samples"`
		Position float64 `json:"position"`
	}
	if err := json.Unmarshal(result, &report); err != nil || report.Error != "" || report.Position < 3 || report.Samples == 0 {
		t.Fatalf("native playback result: error = %v\n%s", err, result)
	}
	select {
	case <-stalledCanceled:
	default:
		t.Fatal("stalled upstream request was not canceled")
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(requestedRanges) != 2 || requestedRanges[0].start != 0 || requestedRanges[1].start != prefixSize || requestedRanges[1].end != int64(len(payload)-1) {
		t.Fatalf("native playback ranges = %+v; want one exact resume at byte %d", requestedRanges, prefixSize)
	}
	t.Logf("native playback advanced to %.3fs after a stalled %d-byte prefix and one exact range resumption", report.Position, prefixSize)
}

func TestNativePlaybackClassifiesInitialRangeFailures(t *testing.T) {
	executable, err := exec.LookPath("mpv.exe")
	if err != nil {
		t.Skip("mpv.exe is not available on PATH")
	}
	for _, sample := range []struct {
		name         string
		status       int
		invalidRange bool
		recovers     bool
	}{{name: "transient-503", status: http.StatusServiceUnavailable, recovers: true}, {name: "permanent-403", status: http.StatusForbidden}, {name: "permanent-404", status: http.StatusNotFound}, {name: "invalid-206", status: http.StatusPartialContent, invalidRange: true}} {
		t.Run(sample.name, func(t *testing.T) {
			payload := playbackWAV()
			var upstreamCalls, localCalls atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := upstreamCalls.Add(1)
				if !sample.recovers || call <= maxChunkAttempts {
					if sample.invalidRange {
						w.Header().Set("Content-Range", "invalid")
					}
					w.WriteHeader(sample.status)
					return
				}
				span, err := parseRange(r.Header.Get("Range"), int64(len(payload)))
				if err != nil || span.start != 0 {
					t.Errorf("native initial retry range = %+v, error = %v", span, err)
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", span.start, span.end, len(payload)))
				w.Header().Set("Content-Length", fmt.Sprint(span.end-span.start+1))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(payload[span.start : span.end+1])
			}))
			defer upstream.Close()
			gateway, err := StartDynamic(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close()
			gateway.client.CloseIdleConnections()
			gateway.client = upstream.Client()
			if err := gateway.Add([]Resource{{Name: "audio.wav", URL: upstream.URL, MIME: "audio/wav", Size: int64(len(payload)), Ranged: true}}); err != nil {
				t.Fatal(err)
			}
			gateway.httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				localCalls.Add(1)
				gateway.ServeHTTP(w, r)
			})
			position, reason := observeNativeRangeOpening(t, executable, gateway.URL("audio.wav"))
			if sample.recovers {
				if reason != "progress" || position < 0.5 || upstreamCalls.Load() != maxChunkAttempts+1 || localCalls.Load() != 2 {
					t.Fatalf("position = %.3f, reason = %q, upstream = %d, local = %d", position, reason, upstreamCalls.Load(), localCalls.Load())
				}
			} else if reason != "error" || upstreamCalls.Load() != 1 || localCalls.Load() != 1 {
				t.Fatalf("permanent opening failure: position = %.3f, reason = %q, upstream = %d, local = %d", position, reason, upstreamCalls.Load(), localCalls.Load())
			}
			t.Logf("native initial opening: position = %.3fs, reason = %s, upstream = %d, local = %d", position, reason, upstreamCalls.Load(), localCalls.Load())
		})
	}
}

func observeNativeRangeOpening(t *testing.T, executable, source string) (float64, string) {
	t.Helper()
	directory := t.TempDir()
	resultPath := filepath.Join(directory, "opening.json")
	quotedResultPath, err := json.Marshal(filepath.ToSlash(resultPath))
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`local mp = require "mp"
local utils = require "mp.utils"
local finished = false
local function finish(reason)
    if finished then return end
    finished = true
    local output = assert(io.open(%s, "wb"))
    output:write(utils.format_json({reason = reason, position = mp.get_property_number("time-pos", -1)}))
    output:close()
    mp.commandv("quit", 0)
end
mp.register_event("end-file", function(event) finish(event.reason) end)
mp.add_periodic_timer(0.05, function()
    if mp.get_property_number("time-pos", -1) >= 0.5 then finish("progress") end
end)
mp.add_timeout(3, function() finish("deadline") end)
`, quotedResultPath)
	scriptPath := filepath.Join(directory, "opening.lua")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, runErr := exec.CommandContext(ctx, executable, "--no-config", "--load-scripts=no", "--idle=yes", "--vo=null", "--ao=null", "--input-terminal=no", "--media-controls=no", "--ytdl=no", "--script="+scriptPath, source).CombinedOutput()
	result, readErr := os.ReadFile(resultPath)
	if runErr != nil || readErr != nil {
		t.Fatalf("native initial opening: execution = %v, result = %v\n%s\n%s", runErr, readErr, output, result)
	}
	var report struct {
		Reason   string  `json:"reason"`
		Position float64 `json:"position"`
	}
	if err := json.Unmarshal(result, &report); err != nil {
		t.Fatal(err)
	}
	return report.Position, report.Reason
}

func playbackWAV() []byte {
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
