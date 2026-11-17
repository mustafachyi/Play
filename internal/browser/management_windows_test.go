package browser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestManagementMenuRepair(t *testing.T) {
	for _, scenario := range []string{"disabled", "missing manifest", "missing registration"} {
		t.Run(scenario, func(t *testing.T) {
			manager, registration, mpvPath := lifecycleFixture(t)
			if scenario != "disabled" {
				if err := manager.enable(mpvPath, false); err != nil {
					t.Fatal(err)
				}
				if scenario == "missing manifest" {
					if err := os.Remove(manager.manifestPath()); err != nil {
						t.Fatal(err)
					}
				} else {
					registration.value = ""
				}
			}
			var output bytes.Buffer
			err := runManagement(context.Background(), manager, nil, strings.NewReader("4\n"), &output, io.Discard)
			if !strings.Contains(output.String(), "4. Repair\n") {
				t.Fatalf("repair is absent from the menu: %s", output.String())
			}
			if scenario == "disabled" {
				if err == nil || !strings.Contains(err.Error(), "disabled") {
					t.Fatalf("repair must require enabled integration: %v", err)
				}
				if registration.value != "" {
					t.Fatal("repair enabled integration without consent")
				}
				if _, err := os.Stat(manager.directory); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("disabled repair created integration files: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			status := assertConnectionState(t, manager, "enabled")
			if status.MPVPath != mpvPath || registration.value != manager.manifestPath() {
				t.Fatalf("repair changed the selected mpv or failed to restore registration: %+v", status)
			}
			if strings.Contains(output.String(), "mpv.exe path") {
				t.Fatal("repair prompted for a new mpv location")
			}
		})
	}
}

type managementInputProbe struct {
	io.ReadCloser
	reads chan struct{}
}

func (input managementInputProbe) Read(buffer []byte) (int, error) {
	input.reads <- struct{}{}
	return input.ReadCloser.Read(buffer)
}

func TestManagementCancellationInterruptsPrompt(t *testing.T) {
	for _, stage := range []string{"menu", "mpv selection"} {
		t.Run(stage, func(t *testing.T) {
			manager, registration, _ := lifecycleFixture(t)
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			input := managementInputProbe{ReadCloser: reader, reads: make(chan struct{}, 4)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				result <- runManagement(ctx, manager, nil, input, io.Discard, io.Discard)
			}()
			waitForRead := func() {
				t.Helper()
				select {
				case <-input.reads:
				case <-time.After(5 * time.Second):
					t.Fatal("management prompt did not request input")
				}
			}
			waitForRead()
			if stage == "mpv selection" {
				if _, err := io.WriteString(writer, "1\n"); err != nil {
					t.Fatal(err)
				}
				waitForRead()
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled management returned %v", err)
				}
			case <-time.After(time.Second):
				_ = reader.Close()
				<-result
				t.Fatal("canceled management remained blocked waiting for input")
			}
			if registration.value != "" {
				t.Fatal("canceled management changed Firefox registration")
			}
			if _, err := os.Stat(manager.directory); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canceled management wrote integration state: %v", err)
			}
		})
	}
}
