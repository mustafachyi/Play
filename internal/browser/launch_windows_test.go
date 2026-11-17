package browser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNativeLaunchArgumentsAndEnvironment(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "arguments.json")
	t.Setenv("PLAY_BROWSER_NATIVE_TEST_OUTPUT", output)
	t.Setenv("PLAY_BROWSER_NATIVE_TEST_CHILD", "capture")
	t.Setenv("NoDefaultCurrentDirectoryInExePath", "0")
	mpv := nativeTestMPV(t)
	for _, mode := range []string{"video", "audio"} {
		if err := launchNativePlayback(context.Background(), executable, mpv, nativeRequest{URL: nativeTestURL + "&list=PLexample", Mode: mode}); err != nil {
			t.Fatal(err)
		}
		payload, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		var actual struct {
			Args []string
			Path string
			Safe string
		}
		if err := json.Unmarshal(payload, &actual); err != nil {
			t.Fatal(err)
		}
		expected := []string{nativeTestURL + "&list=PLexample"}
		if mode == "audio" {
			expected = append([]string{"-a"}, expected...)
		}
		if !reflect.DeepEqual(actual.Args, expected) || !strings.HasPrefix(actual.Path, filepath.Dir(mpv)+";") || actual.Safe != "1" {
			t.Fatalf("launch arguments/environment: %+v", actual)
		}
	}
}

func TestNativeLaunchFailureAndCancellation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mpv := nativeTestMPV(t)
	request := nativeRequest{URL: nativeTestURL, Mode: "video"}
	t.Setenv("PLAY_BROWSER_NATIVE_TEST_CHILD", "fail")
	err = launchNativePlayback(context.Background(), executable, mpv, request)
	if err == nil || !strings.Contains(err.Error(), "Play startup failed") || !strings.Contains(err.Error(), "failure") || len(err.Error()) > nativeDiagnosticLimit+80 {
		t.Fatalf("unbounded or missing diagnostic: %v", err)
	}
	t.Setenv("PLAY_BROWSER_NATIVE_TEST_CHILD", "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := launchNativePlayback(ctx, executable, mpv, request); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("startup timeout: %v", err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := launchNativePlayback(canceled, executable, mpv, request); err == nil || !strings.Contains(err.Error(), "Startup canceled") || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("startup cancellation: %v", err)
	}
}

func TestNativeEnvironmentSelectsConfiguredMPV(t *testing.T) {
	environment := nativePlaybackEnvironment([]string{`Path=C:\old`, "X=Y", "NoDefaultCurrentDirectoryInExePath=0"}, `C:\chosen`)
	if !reflect.DeepEqual(environment, []string{"X=Y", `PATH=C:\chosen;C:\old`, "NoDefaultCurrentDirectoryInExePath=1"}) {
		t.Fatalf("environment: %v", environment)
	}
	environment = nativePlaybackEnvironment([]string{`PATH=C:\old`}, `C:\chosen;player`)
	for _, entry := range environment {
		if path, ok := strings.CutPrefix(entry, "PATH="); ok {
			if parts := filepath.SplitList(path); !reflect.DeepEqual(parts, []string{`C:\chosen;player`, `C:\old`}) {
				t.Fatalf("configured mpv directory split into multiple entries: %v", parts)
			}
		}
	}
}
