package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
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

func TestNativeLaunchExitAccessDenied(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "launches.txt")
	t.Setenv("PLAY_BROWSER_NATIVE_TEST_OUTPUT", output)
	t.Setenv("PLAY_BROWSER_NATIVE_TEST_CHILD", "exit-access-denied")
	err = launchNativePlayback(context.Background(), executable, nativeTestMPV(t), nativeRequest{URL: nativeTestURL, Mode: "video"})
	if err == nil || !strings.Contains(err.Error(), "exit status 5") {
		t.Fatalf("child exit: %v", err)
	}
	payload, err := os.ReadFile(output)
	if err != nil || string(payload) != "launch\n" {
		t.Fatalf("child launches = %q, read error = %v; want one launch", payload, err)
	}
}

func TestNativeLaunchInRestrictedJob(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if os.Getenv("PLAY_BROWSER_NATIVE_TEST_RESTRICTED_JOB") == "1" {
		command := exec.CommandContext(ctx, executable)
		command.Env = append(os.Environ(), "PLAY_BROWSER_NATIVE_TEST_CHILD=capture")
		command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: nativeBreakawayFromJob | nativeCreateNoWindow}
		if err := command.Start(); !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			if err == nil {
				_ = command.Wait()
			}
			t.Fatalf("restricted job breakaway = %v; want access denied", err)
		}
		t.Run("arguments-and-environment", TestNativeLaunchArgumentsAndEnvironment)
		t.Run("failure-and-cancellation", TestNativeLaunchFailureAndCancellation)
		t.Run("exit-access-denied", TestNativeLaunchExitAccessDenied)
		return
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestNativeLaunchInRestrictedJob$", "-test.v", "-test.timeout=30s")
	command.Env = append(os.Environ(), "PLAY_BROWSER_NATIVE_TEST_CHILD=restricted-job")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: nativeCreateNoWindow}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("restricted-job launch checks: %v\n%s", err, output)
	}
}

func runNativeTestsInRestrictedJob(m *testing.M) int {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	job, _, callErr := kernel32.NewProc("CreateJobObjectW").Call(0, 0)
	if job == 0 {
		_, _ = os.Stderr.WriteString("create restricted test job: " + callErr.Error() + "\n")
		return 2
	}
	defer syscall.CloseHandle(syscall.Handle(job))
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		_, _ = os.Stderr.WriteString("get test process: " + err.Error() + "\n")
		return 2
	}
	assigned, _, callErr := kernel32.NewProc("AssignProcessToJobObject").Call(job, uintptr(process))
	if assigned == 0 {
		_, _ = os.Stderr.WriteString("assign restricted test job: " + callErr.Error() + "\n")
		return 2
	}
	if err := os.Unsetenv("PLAY_BROWSER_NATIVE_TEST_CHILD"); err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		return 2
	}
	if err := os.Setenv("PLAY_BROWSER_NATIVE_TEST_RESTRICTED_JOB", "1"); err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		return 2
	}
	return m.Run()
}
