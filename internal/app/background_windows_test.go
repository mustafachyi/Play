package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"play/internal/youtube"
)

func TestMain(m *testing.M) {
	if os.Getenv("PLAY_TEST_WORKER") != "" && len(os.Args) == 2 && os.Args[1] == "--play-worker" {
		decoder := json.NewDecoder(os.Stdin)
		var request workerRequest
		if decoder.Decode(&request) != nil {
			os.Exit(2)
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

func TestBackgroundReturnsWhileWorkerRemainsAlive(t *testing.T) {
	t.Setenv("PLAY_TEST_WORKER", "success")
	directory := t.TempDir()
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
	if err := launchBackground(ctx, executable, youtube.Reference{VideoID: appTestID}, false, &out); err != nil {
		t.Fatal(err)
	}
	logPath := strings.TrimSpace(strings.TrimPrefix(out.String(), "Playback launched. Diagnostics: "))
	t.Cleanup(func() { _ = os.Remove(logPath) })
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
	err = launchBackground(context.Background(), executable, youtube.Reference{VideoID: appTestID}, true, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "simulated startup failure") {
		t.Fatalf("error=%v", err)
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
	err = launchBackground(ctx, executable, youtube.Reference{VideoID: appTestID}, false, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
}

func TestWaitKeepsPlaybackInForeground(t *testing.T) {
	deps := testDependencies(testItem(), nil, nil, nil)
	background := false
	deps.background = func(context.Context, youtube.Reference, bool, io.Writer) error {
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
		t.Fatalf("background=%v error=%v", background, err)
	}
}
