package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"play/internal/youtube"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--play-worker-child" {
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}
	if os.Getenv("PLAY_TEST_WORKER") == "log-ownership" && len(os.Args) == 2 && os.Args[1] == "--play-worker" {
		if RunWorker(context.Background()) != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("PLAY_TEST_WORKER") != "" && len(os.Args) == 2 && os.Args[1] == "--play-worker" {
		decoder := json.NewDecoder(os.Stdin)
		var request workerRequest
		if decoder.Decode(&request) != nil {
			os.Exit(2)
		}
		if os.Getenv("PLAY_TEST_WORKER") == "ignore-input" {
			time.Sleep(3 * time.Second)
			os.Exit(0)
		}
		if os.Getenv("PLAY_TEST_WORKER") == "reject-ack" {
			_ = os.Stdin.Close()
			fmt.Fprintln(os.Stdout, "ready")
			_ = os.Stdout.Close()
			time.Sleep(3 * time.Second)
			os.Exit(0)
		}
		if os.Getenv("PLAY_TEST_WORKER") == "cleanup-child" {
			ctx, cancel := context.WithCancel(context.Background())
			child := exec.CommandContext(ctx, os.Args[0], "--play-worker-child")
			child.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
			if child.Start() != nil {
				os.Exit(5)
			}
			if os.WriteFile(os.Getenv("PLAY_TEST_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
				cancel()
				_ = child.Wait()
				os.Exit(5)
			}
			var accepted bool
			_ = decoder.Decode(&accepted)
			cancel()
			_ = child.Wait()
			if os.WriteFile(os.Getenv("PLAY_TEST_CHILD_CLEANUP"), []byte("cleaned"), 0600) != nil {
				os.Exit(5)
			}
			os.Exit(0)
		}
		if request.AudioOnly {
			fmt.Fprintln(os.Stderr, "simulated startup failure")
			os.Exit(1)
		}
		if os.Getenv("PLAY_TEST_WORKER") == "cancel" {
			var accepted bool
			_ = decoder.Decode(&accepted)
			os.Exit(0)
		}
		fmt.Fprintln(os.Stdout, "ready")
		_ = os.Stdout.Close()
		var accepted bool
		if decoder.Decode(&accepted) != nil || !accepted {
			os.Exit(3)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(os.Getenv("PLAY_TEST_RELEASE")); err == nil {
				_ = os.WriteFile(os.Getenv("PLAY_TEST_DONE"), []byte("done"), 0600)
				os.Exit(0)
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(4)
	}
	os.Exit(m.Run())
}

func TestWorkerCannotDeleteUnrelatedFileWhenStderrIsNull(t *testing.T) {
	t.Setenv("PLAY_TEST_WORKER", "log-ownership")
	path := filepath.Join(t.TempDir(), "unrelated.txt")
	contents := []byte("preserve unrelated data")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(workerRequest{LogPath: path})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "--play-worker")
	command.Stdin = strings.NewReader(string(payload) + "\nfalse\n")
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	command.Stderr = null
	var exitError *exec.ExitError
	if err := command.Run(); !errors.As(err, &exitError) || exitError.ExitCode() != 1 || ctx.Err() != nil {
		t.Fatalf("worker error = %v; want exit code 1 and no context error", err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(contents) {
		t.Fatalf("unrelated file content = %q, read error = %v; want %q", actual, err, contents)
	}
}

func TestBackgroundReturnsWhileWorkerRemainsAlive(t *testing.T) {
	t.Setenv("PLAY_TEST_WORKER", "success")
	directory := t.TempDir()
	t.Setenv("TMP", directory)
	release := filepath.Join(directory, "release")
	done := filepath.Join(directory, "done")
	t.Setenv("PLAY_TEST_RELEASE", release)
	t.Setenv("PLAY_TEST_DONE", done)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deps := testDependencies(testItem(), nil, nil, nil)
	deps.background = func(ctx context.Context, reference youtube.Reference, audioOnly bool) error {
		return launchBackground(ctx, executable, reference, audioOnly)
	}
	if err := run(ctx, "test", []string{appTestID}, strings.NewReader(""), &out, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("successful launch produced output: %q", out.String())
	}
	if _, err := os.Stat(done); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("worker did not remain active after launch returned")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(done); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("worker did not finish after parent returned")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestBackgroundReturnsStartupFailure(t *testing.T) {
	t.Setenv("PLAY_TEST_WORKER", "failure")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = launchBackground(context.Background(), executable, youtube.Reference{VideoID: appTestID}, true)
	if err == nil || !strings.Contains(err.Error(), "simulated startup failure") {
		t.Fatalf("error = %v", err)
	}
}

func TestBackgroundCancellationStopsWorker(t *testing.T) {
	t.Setenv("PLAY_TEST_WORKER", "cancel")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err = launchBackground(ctx, executable, youtube.Reference{VideoID: appTestID}, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestBackgroundCancellationStopsWorkerIgnoringInput(t *testing.T) {
	t.Setenv("PLAY_TEST_WORKER", "ignore-input")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = launchBackground(ctx, executable, youtube.Reference{VideoID: appTestID}, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("cancellation waited for an uncooperative worker: %v", elapsed)
	}
}

func TestBackgroundFailedAcknowledgementStopsWorker(t *testing.T) {
	t.Setenv("PLAY_TEST_WORKER", "reject-ack")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = launchBackground(context.Background(), executable, youtube.Reference{VideoID: appTestID}, false)
	if err == nil || !strings.Contains(err.Error(), "acknowledge playback worker") {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("failed acknowledgement waited for an uncooperative worker: %v", elapsed)
	}
}

func TestBackgroundCancellationAllowsOwnedChildCleanup(t *testing.T) {
	t.Setenv("PLAY_TEST_WORKER", "cleanup-child")
	directory := t.TempDir()
	pidPath := filepath.Join(directory, "child-pid")
	cleanupPath := filepath.Join(directory, "child-cleanup")
	t.Setenv("PLAY_TEST_CHILD_PID", pidPath)
	t.Setenv("PLAY_TEST_CHILD_CLEANUP", cleanupPath)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- launchBackground(ctx, executable, youtube.Reference{VideoID: appTestID}, false) }()
	var child syscall.Handle
	for child == 0 {
		body, err := os.ReadFile(pidPath)
		if err == nil {
			pid, err := strconv.Atoi(string(body))
			if err == nil {
				child, err = syscall.OpenProcess(syscall.SYNCHRONIZE|syscall.PROCESS_TERMINATE, false, uint32(pid))
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		if child != 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("worker did not start its owned child")
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Cleanup(func() {
		if state, _ := syscall.WaitForSingleObject(child, 0); state == syscall.WAIT_TIMEOUT {
			_ = syscall.TerminateProcess(child, 1)
		}
		_ = syscall.CloseHandle(child)
	})
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v; want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker shutdown did not finish")
	}
	if _, err := os.Stat(cleanupPath); err != nil {
		t.Fatalf("worker did not finish cooperative child cleanup: %v", err)
	}
	if state, err := syscall.WaitForSingleObject(child, 0); err != nil || state != syscall.WAIT_OBJECT_0 {
		t.Fatalf("owned child is still active: state = %d, error = %v", state, err)
	}
}

func TestWaitKeepsPlaybackInForeground(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	background := false
	deps.background = func(context.Context, youtube.Reference, bool) error {
		background = true
		return nil
	}
	if err := run(context.Background(), "test", []string{"-wait", appTestID}, strings.NewReader(""), io.Discard, io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	if background {
		t.Fatal("-wait launched a background worker")
	}
	if err := run(context.Background(), "test", []string{appTestID}, strings.NewReader(""), io.Discard, io.Discard, deps); err != nil || !background {
		t.Fatalf("background = %v, error = %v", background, err)
	}
}
