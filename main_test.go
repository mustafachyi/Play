package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlaylistPageHelperExitStatus(t *testing.T) {
	if target := os.Getenv("PLAY_TEST_PLAYLIST_HELPER_URL"); target != "" {
		os.Args = []string{os.Args[0], "-playlist-page", target, os.Getenv("PLAY_TEST_PLAYLIST_HELPER_DIRECTORY")}
		main()
		os.Exit(0)
	}
	for _, test := range []struct {
		name   string
		status int
		code   int
	}{{"cooldown", http.StatusServiceUnavailable, 75}, {"temporary", http.StatusBadGateway, 1}, {"success", http.StatusOK, 0}} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				if test.status == http.StatusOK {
					_, _ = io.WriteString(w, "#EXTM3U\n")
				}
			}))
			defer server.Close()
			directory, err := os.MkdirTemp("", "play-pages-")
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Dir(directory) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(directory), "play-pages-") {
				t.Fatal("unexpected helper fixture directory")
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestPlaylistPageHelperExitStatus$")
			command.Env = append(os.Environ(), "PLAY_TEST_PLAYLIST_HELPER_URL="+server.URL+"/0123456789abcdef0123456789abcdef/page/0002.m3u", "PLAY_TEST_PLAYLIST_HELPER_DIRECTORY="+directory)
			output, runErr := command.CombinedOutput()
			actualCode := 0
			if runErr != nil {
				var exitError *exec.ExitError
				if !errors.As(runErr, &exitError) {
					t.Fatalf("helper subprocess failed: %v\n%s", runErr, output)
				}
				actualCode = exitError.ExitCode()
			}
			if actualCode != test.code || ctx.Err() != nil {
				t.Fatalf("helper exit = %d; want %d; context error = %v\n%s", actualCode, test.code, ctx.Err(), output)
			}
		})
	}
}
