package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const nativeBreakawayFromJob = 0x01000000
const nativeCreateNoWindow = 0x08000000

type nativeDiagnosticBuffer struct {
	data []byte
}

func (buffer *nativeDiagnosticBuffer) Write(data []byte) (int, error) {
	length := len(data)
	remaining := nativeDiagnosticLimit - len(buffer.data)
	if len(data) > remaining {
		data = data[:remaining]
	}
	buffer.data = append(buffer.data, data...)
	return length, nil
}

func launchNativePlayback(ctx context.Context, executable, mpvPath string, request nativeRequest) error {
	args := []string{request.URL}
	if request.Mode == "audio" {
		args = []string{"-a", request.URL}
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: nativeBreakawayFromJob | nativeCreateNoWindow}
	command.Dir = filepath.Dir(executable)
	command.Env = nativePlaybackEnvironment(os.Environ(), filepath.Dir(mpvPath))
	command.Stdout = io.Discard
	diagnostics := &nativeDiagnosticBuffer{}
	command.Stderr = diagnostics
	command.WaitDelay = time.Second
	err := command.Run()
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return errors.New("Startup canceled. Check whether mpv opened before retrying.")
		}
		return errors.New("Startup timed out. Check whether mpv opened before retrying.")
	}
	if err != nil {
		detail := strings.TrimSpace(string(diagnostics.data))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("Play startup failed. Check whether mpv opened before retrying.\n\n%s", detail)
	}
	return nil
}

func nativePlaybackEnvironment(environment []string, mpvDirectory string) []string {
	result := make([]string, 0, len(environment)+1)
	path := mpvDirectory
	if strings.ContainsRune(path, ';') {
		path = `"` + path + `"`
	}
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "PATH") {
			path += ";" + value
		} else if !strings.EqualFold(key, "NoDefaultCurrentDirectoryInExePath") {
			result = append(result, entry)
		}
	}
	return append(result, "PATH="+path, "NoDefaultCurrentDirectoryInExePath=1")
}
