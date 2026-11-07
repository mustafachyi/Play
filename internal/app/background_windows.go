package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"play/internal/mpv"
	"play/internal/youtube"
)

type workerRequest struct {
	Reference youtube.Reference
	AudioOnly bool
	LogPath   string
}

func startBackground(ctx context.Context, reference youtube.Reference, audioOnly bool) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate playback worker: %w", err)
	}
	return launchBackground(ctx, executable, reference, audioOnly)
}

func launchBackground(ctx context.Context, executable string, reference youtube.Reference, audioOnly bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	logFile, err := os.CreateTemp("", "play-session-*.log")
	if err != nil {
		return fmt.Errorf("create playback log: %w", err)
	}
	retainLog := false
	defer func() {
		_ = logFile.Close()
		if !retainLog {
			_ = os.Remove(logFile.Name())
		}
	}()
	payload, err := json.Marshal(workerRequest{Reference: reference, AudioOnly: audioOnly, LogPath: logFile.Name()})
	if err != nil {
		return err
	}
	command := exec.Command(executable, "--play-worker")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000 | syscall.CREATE_NEW_PROCESS_GROUP}
	command.Stderr = logFile
	input, err := command.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	ready, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	defer ready.Close()
	if err := command.Start(); err != nil {
		return fmt.Errorf("start playback worker: %w", err)
	}
	_ = logFile.Close()
	if _, err := input.Write(append(payload, '\n')); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("send playback worker request: %w", err)
	}
	result := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(ready, 64)).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = errors.New("invalid playback worker acknowledgement")
		}
		result <- err
	}()
	select {
	case <-ctx.Done():
		_ = input.Close()
		_ = command.Wait()
		return ctx.Err()
	case err := <-result:
		if err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			body, readErr := os.ReadFile(logFile.Name())
			if readErr == nil && len(bytes.TrimSpace(body)) > 0 {
				return errors.New(strings.TrimSpace(string(body)))
			}
			retainLog = true
			return fmt.Errorf("playback worker failed before launching mpv; log: %s: %w", logFile.Name(), err)
		}
	}
	if _, err := io.WriteString(input, "true\n"); err != nil {
		_ = input.Close()
		_ = command.Wait()
		return fmt.Errorf("acknowledge playback worker: %w", err)
	}
	retainLog = true
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release playback worker: %w", err)
	}
	return nil
}

func RunWorker(ctx context.Context) (runErr error) {
	var request workerRequest
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 16<<10))
	if err := decoder.Decode(&request); err != nil {
		fmt.Fprintf(os.Stderr, "read playback worker request: %v\n", err)
		return err
	}
	defer func() {
		if runErr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", runErr)
		}
		info, err := os.Stderr.Stat()
		_ = os.Stderr.Close()
		if err == nil && info.Size() == 0 {
			_ = os.Remove(request.LogPath)
		}
	}()
	deps := defaultDependencies()
	defer deps.closeSource()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	acknowledged := make(chan struct{})
	go func() {
		var accepted bool
		if err := decoder.Decode(&accepted); err != nil || !accepted {
			cancel()
			return
		}
		close(acknowledged)
	}()
	started := func() error {
		_, err := io.WriteString(os.Stdout, "ready\n")
		_ = os.Stdout.Close()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-acknowledged:
			return nil
		}
	}
	deps.play = func(ctx context.Context, request mpv.Request) error {
		request.Started = started
		return mpv.Run(ctx, request)
	}
	deps.playPlaylist = func(ctx context.Context, request mpv.PlaylistRequest) error {
		request.Started = started
		return mpv.RunPlaylist(ctx, request)
	}
	mpvPath, err := deps.findMPV()
	if err != nil {
		return err
	}
	if request.Reference.PlaylistID != "" {
		return runPlaylist(ctx, request.Reference, request.AudioOnly, mpvPath, os.Stderr, deps)
	}
	return runVideo(ctx, request.Reference, request.AudioOnly, mpvPath, os.Stderr, deps)
}
