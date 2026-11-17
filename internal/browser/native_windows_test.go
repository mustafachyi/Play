package browser

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const nativeTestURL = "https://www.youtube.com/watch?v=C-QcjB6B3aU"

func TestMain(m *testing.M) {
	if behavior := os.Getenv("PLAY_BROWSER_NATIVE_TEST_CHILD"); behavior != "" {
		switch behavior {
		case "capture":
			payload, err := json.Marshal(struct {
				Args []string
				Path string
				Safe string
			}{os.Args[1:], os.Getenv("PATH"), os.Getenv("NoDefaultCurrentDirectoryInExePath")})
			if err != nil || os.WriteFile(os.Getenv("PLAY_BROWSER_NATIVE_TEST_OUTPUT"), payload, 0600) != nil {
				os.Exit(2)
			}
		case "fail":
			_, _ = os.Stderr.WriteString(strings.Repeat("failure ", 10000))
			os.Exit(1)
		case "wait":
			time.Sleep(time.Minute)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func frameNativeTest(payload string) []byte {
	var output bytes.Buffer
	_ = binary.Write(&output, binary.LittleEndian, uint32(len(payload)))
	output.WriteString(payload)
	return output.Bytes()
}

func nativeTestMPV(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mpv.exe")
	if err := os.WriteFile(path, []byte("test executable"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativePlaybackRequests(t *testing.T) {
	for _, mode := range []string{"video", "audio"} {
		for _, url := range []string{nativeTestURL, nativeTestURL + "&list=PLexample", "https://www.youtube.com/playlist?list=OLAK5uy_example"} {
			payload, err := json.Marshal(map[string]any{"version": 1, "url": url, "mode": mode})
			if err != nil {
				t.Fatal(err)
			}
			request, err := readNativeRequest(bytes.NewReader(frameNativeTest(string(payload))))
			if err != nil || request.URL != url || request.Mode != mode || request.Action != "" {
				t.Fatalf("valid request: %+v, %v", request, err)
			}
		}
	}
	request, err := readNativeRequest(bytes.NewReader(frameNativeTest(`{"version":1,"action":"status"}`)))
	if err != nil || request.Action != "status" || request.URL != "" || request.Mode != "" {
		t.Fatalf("status request: %+v, %v", request, err)
	}
}

func TestNativeRejectsInvalidSchema(t *testing.T) {
	for _, payload := range []string{
		`null`, `[]`, `{}`, `{"version":2,"action":"status"}`,
		`{"version":1,"url":"` + nativeTestURL + `","mode":"shell"}`,
		`{"version":1,"url":"` + nativeTestURL + `","mode":"video","command":"cmd.exe"}`,
		`{"version":1,"url":"` + nativeTestURL + `","mode":"video"} {}`,
		`{"version":1,"action":"status","url":"` + nativeTestURL + `"}`,
		`{"version":1,"action":"status","mode":null}`,
		`{"version":1,"action":null,"url":"` + nativeTestURL + `","mode":"video"}`,
		`{"version":1,"action":"status","action":"status"}`,
		`{"Version":1,"action":"status"}`,
		`{"version":"1","action":"status"}`,
		`{"version":1,"action":"enable"}`,
		`{"version":1,"action":"status"`,
		"{\"version\":1,\"action\":\"status\",\"\xff\":1}",
	} {
		if _, err := readNativeRequest(bytes.NewReader(frameNativeTest(payload))); err == nil {
			t.Errorf("accepted invalid schema: %s", payload)
		}
	}
}

func TestNativePlaylistIDLengthBoundaries(t *testing.T) {
	for _, length := range []int{1, 2, 128, 129, 200, 201} {
		playlistID := strings.Repeat("P", length)
		for _, target := range []string{
			"https://www.youtube.com/playlist?list=" + playlistID,
			nativeTestURL + "&list=" + playlistID,
		} {
			payload, err := json.Marshal(map[string]any{"version": 1, "url": target, "mode": "video"})
			if err != nil {
				t.Fatal(err)
			}
			request, err := readNativeRequest(bytes.NewReader(frameNativeTest(string(payload))))
			valid := length >= 2 && length <= 200
			if (err == nil) != valid || valid && request.URL != target {
				t.Errorf("playlist ID length %d: request=%+v, error=%v", length, request, err)
			}
		}
	}
}

func TestNativeRejectsUnsupportedURLs(t *testing.T) {
	for _, url := range []string{
		"https://evil.example/watch?v=C-QcjB6B3aU", "file:///C:/Windows/cmd.exe", "-help",
		nativeTestURL + "&list=RDexample", nativeTestURL + "&v=lJvRohYSrZM", nativeTestURL + "&t=5",
		nativeTestURL + "&list=PLone&list=PLtwo", nativeTestURL + "&list=bad%22%26calc",
		nativeTestURL + "#fragment", nativeTestURL + "\n", "https://www.youtube.com/playlist?list=RDexample",
		"https://www.youtube.com:443/watch?v=C-QcjB6B3aU", "https://user@www.youtube.com/watch?v=C-QcjB6B3aU",
	} {
		payload, err := json.Marshal(map[string]any{"version": 1, "url": url, "mode": "video"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readNativeRequest(bytes.NewReader(frameNativeTest(string(payload)))); err == nil {
			t.Errorf("accepted unsafe URL: %q", url)
		}
	}
}

func TestNativeFrameLimitsAndResponse(t *testing.T) {
	for _, data := range [][]byte{nil, {1, 2}, {0, 0, 0, 0}, {255, 255, 255, 255}, {10, 0, 0, 0, '{'}} {
		if _, err := readNativeRequest(bytes.NewReader(data)); err == nil {
			t.Errorf("accepted incomplete or oversized frame: %v", data)
		}
	}
	var output bytes.Buffer
	response := nativeResponse{Version: 1, Error: "Could not open: 日本語"}
	if err := writeNativeResponse(&output, response); err != nil {
		t.Fatal(err)
	}
	var size uint32
	if err := binary.Read(&output, binary.LittleEndian, &size); err != nil || int(size) != output.Len() {
		t.Fatalf("invalid UTF-8 byte framing: %d, %v", size, err)
	}
	var actual nativeResponse
	if err := json.NewDecoder(&output).Decode(&actual); err != nil || !reflect.DeepEqual(actual, response) {
		t.Fatalf("response: %+v, %v", actual, err)
	}
	if output.Len() != 0 {
		t.Fatalf("unexpected extra output: %q", output.String())
	}
	reader, writer := io.Pipe()
	_ = reader.Close()
	_ = writer.Close()
	if err := writeNativeResponse(writer, response); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write failure: %v", err)
	}
}

func TestNativeCallerAndDispatch(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "youtube_play.json")
	valid := []string{"play.exe", manifest, extensionID}
	if !IsNativeInvocation(valid) || validateNativeCaller(valid, manifest) != nil {
		t.Fatal("valid native invocation rejected")
	}
	for _, args := range [][]string{
		{"play.exe", manifest},
		{"play.exe", manifest, "other@extension"},
		{"play.exe", filepath.Join(filepath.Dir(manifest), "other.json"), extensionID},
		{"play.exe", "youtube_play.json", extensionID},
		{"play.exe", manifest, extensionID, "extra"},
	} {
		if !IsNativeInvocation(args) || validateNativeCaller(args, manifest) == nil {
			t.Errorf("malformed native invocation was not isolated: %v", args)
		}
	}
	for _, args := range [][]string{nil, {"play.exe"}, {"play.exe", nativeTestURL}, {"play.exe", "-a", nativeTestURL}, {"play.exe", "-browser", "enable"}, {"play.exe", "--play-worker"}} {
		if IsNativeInvocation(args) {
			t.Errorf("normal invocation routed as native: %v", args)
		}
	}
}

func TestRunNativeValidatesBeforeWritingAndFramesFailure(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	manifest, err := nativeManifestPath()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"play.exe", manifest, extensionID}
	var output bytes.Buffer
	if err := RunNative(context.Background(), "0.1.4", args[:1], bytes.NewReader(frameNativeTest(`{}`)), &output); err == nil || output.Len() != 0 {
		t.Fatalf("invalid caller: error=%v, output=%q", err, output.Bytes())
	}
	if err := RunNative(context.Background(), "0.1.4", args, bytes.NewReader(frameNativeTest(`{}`)), &output); err != nil {
		t.Fatal(err)
	}
	var size uint32
	if err := binary.Read(&output, binary.LittleEndian, &size); err != nil || int(size) != output.Len() {
		t.Fatalf("protocol failure framing: size=%d, error=%v", size, err)
	}
	var response nativeResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Version != 1 || response.Error == "" {
		t.Fatalf("protocol failure response: %+v", response)
	}
}

func TestNativeHealthIsReadOnly(t *testing.T) {
	mpv := nativeTestMPV(t)
	for _, test := range []struct {
		name   string
		status connectionStatus
		err    error
		state  string
		ready  bool
	}{
		{"ready", connectionStatus{State: "enabled", MPVPath: mpv}, nil, "ready", true},
		{"missing-mpv", connectionStatus{State: "enabled", MPVPath: filepath.Join(t.TempDir(), "mpv.exe")}, nil, "mpv-missing", false},
		{"disabled", connectionStatus{State: "disabled"}, nil, "disabled", false},
		{"conflict", connectionStatus{State: "conflict", Detail: "Another Play copy owns the connection."}, nil, "needs-attention", false},
		{"inspection-error", connectionStatus{}, errors.New("Cannot read registration."), "needs-attention", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := serveNative(context.Background(), "0.1.4", bytes.NewReader(frameNativeTest(`{"version":1,"action":"status"}`)), nativeDependencies{
				inspect: func() (connectionStatus, error) { return test.status, test.err },
				launch: func(context.Context, string, nativeRequest) error {
					t.Fatal("health request launched playback")
					return nil
				},
			})
			if response.State != test.state || response.OK != test.ready || response.HostVersion != "0.1.4" || response.MPVAvailable == nil {
				t.Fatalf("health response: %+v", response)
			}
			if test.ready && (!*response.MPVAvailable || response.Error != "") {
				t.Fatalf("ready response: %+v", response)
			}
			if !test.ready && response.Error == "" {
				t.Fatalf("missing recovery detail: %+v", response)
			}
			if test.state == "mpv-missing" && strings.Contains(response.Error, "repair") {
				t.Fatalf("missing mpv must not suggest repairing its old path: %s", response.Error)
			}
		})
	}
}

func TestNativePlaybackUsesValidatedConnection(t *testing.T) {
	mpv := nativeTestMPV(t)
	request := frameNativeTest(`{"version":1,"url":"` + nativeTestURL + `","mode":"audio"}`)
	launched := false
	deps := nativeDependencies{
		inspect: func() (connectionStatus, error) { return connectionStatus{State: "enabled", MPVPath: mpv}, nil },
		launch: func(ctx context.Context, path string, request nativeRequest) error {
			launched = true
			if path != mpv || request.Mode != "audio" || request.URL != nativeTestURL || ctx.Err() != nil {
				t.Fatalf("wrong launch inputs: %q, %+v", path, request)
			}
			return nil
		},
	}
	response := serveNative(context.Background(), "0.1.4", bytes.NewReader(request), deps)
	if !launched || !response.OK || response.State != "" || response.HostVersion != "" || response.MPVAvailable != nil {
		t.Fatalf("playback response: %+v, launched=%t", response, launched)
	}
	for _, state := range []string{"disabled", "conflict", "needs-attention"} {
		launched = false
		deps.inspect = func() (connectionStatus, error) { return connectionStatus{State: state}, nil }
		response := serveNative(context.Background(), "0.1.4", bytes.NewReader(request), deps)
		if launched || response.OK || response.Error == "" {
			t.Fatalf("played without enabled ownership: state=%s, response=%+v", state, response)
		}
	}
}

func TestNativeRequestCancellationClosesIncompletePipe(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := readNativeRequestContext(ctx, reader)
		result <- err
	}()
	if _, err := writer.Write([]byte{10, 0, 0, 0, '{'}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("incomplete native request did not stop")
	}
}

func TestNativeErrorBoundPreservesUTF8(t *testing.T) {
	detail := boundNativeError(errors.New(strings.Repeat("日", 1000)))
	if len(detail) > nativeDiagnosticLimit || !utf8.ValidString(detail) || len(detail) == 0 {
		t.Fatalf("invalid bounded error: bytes=%d, valid=%t", len(detail), utf8.ValidString(detail))
	}
}

func FuzzNativeRequest(f *testing.F) {
	f.Add(frameNativeTest(`{"version":1,"url":"` + nativeTestURL + `","mode":"video"}`))
	f.Add(frameNativeTest(`{"version":1,"action":"status"}`))
	f.Add([]byte{255, 255, 255, 255})
	f.Fuzz(func(t *testing.T, data []byte) {
		request, err := readNativeRequest(bytes.NewReader(data))
		if err == nil && (request.Version != 1 || (request.Action != "status" && !nativePlaybackURL.MatchString(request.URL))) {
			t.Fatalf("invalid request accepted: %+v", request)
		}
	})
}
