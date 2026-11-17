package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if logPath := os.Getenv("PLAY_TEST_SCOOP_CALL_LOG"); logPath != "" {
		payload, err := json.Marshal(os.Args[1:])
		if err != nil || os.WriteFile(logPath, payload, 0600) != nil {
			os.Exit(2)
		}
		if os.Getenv("PLAY_TEST_SCOOP_FAIL") == "1" {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestScoopIntegrationHooks(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal("Windows PowerShell is required to verify Scoop integration hooks")
	}
	payload, err := os.ReadFile("bucket/play.json.template")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		PostInstall []string `json:"post_install"`
		Uninstaller struct {
			Script []string `json:"script"`
		} `json:"uninstaller"`
	}
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name    string
		hook    []string
		command string
		global  bool
		fail    bool
		want    string
		failed  bool
		warning bool
	}{
		{"install", manifest.PostInstall, "install", false, false, "refresh", false, false},
		{"update", manifest.PostInstall, "update", false, false, "refresh", false, false},
		{"global-install", manifest.PostInstall, "install", true, false, "", false, false},
		{"optional-refresh-failure", manifest.PostInstall, "update", false, true, "refresh", false, true},
		{"final-uninstall", manifest.Uninstaller.Script, "uninstall", false, false, "remove", false, false},
		{"update-retains-opt-in", manifest.Uninstaller.Script, "update", false, false, "", false, false},
		{"global-uninstall", manifest.Uninstaller.Script, "uninstall", true, false, "", false, false},
		{"cleanup-failure-stops-removal", manifest.Uninstaller.Script, "uninstall", false, true, "remove", true, false},
	} {
		t.Run(check.name, func(t *testing.T) {
			if len(check.hook) == 0 {
				t.Fatal("missing Scoop lifecycle hook")
			}
			directory := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			binary, err := os.ReadFile(executable)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "play.exe"), binary, 0700); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(directory, "invocation.json")
			global := "$false"
			if check.global {
				global = "$true"
			}
			script := fmt.Sprintf("$ErrorActionPreference = 'Stop'\n$dir = '%s'\n$cmd = '%s'\n$old_version = '0.1.3'\n$global = %s\nfunction warn($message) { Write-Output ('OPTIONAL_WARNING: ' + $message) }\n%s\n", strings.ReplaceAll(directory, "'", "''"), check.command, global, strings.Join(check.hook, "\n"))
			scriptPath := filepath.Join(directory, "hook.ps1")
			if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-File", scriptPath)
			fail := "0"
			if check.fail {
				fail = "1"
			}
			command.Env = append(os.Environ(), "PLAY_TEST_SCOOP_CALL_LOG="+logPath, "PLAY_TEST_SCOOP_FAIL="+fail)
			output, runErr := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("Scoop hook did not finish: %v\n%s", ctx.Err(), output)
			}
			if (runErr != nil) != check.failed {
				t.Fatalf("hook result = %v; expected failure %t\n%s", runErr, check.failed, output)
			}
			if strings.Contains(string(output), "OPTIONAL_WARNING:") != check.warning {
				t.Fatalf("unexpected warning result:\n%s", output)
			}
			invocation, readErr := os.ReadFile(logPath)
			if check.want == "" {
				if !os.IsNotExist(readErr) {
					t.Fatalf("hook unexpectedly invoked Play: %s (%v)", invocation, readErr)
				}
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			var args []string
			if err := json.Unmarshal(invocation, &args); err != nil {
				t.Fatal(err)
			}
			wantArgs := []string{"-browser", check.want}
			if check.command == "update" && check.want == "refresh" {
				wantArgs = append(wantArgs, filepath.Join(filepath.Dir(directory), "0.1.3", "play.exe"))
			}
			if !slices.Equal(args, wantArgs) {
				t.Fatalf("Play invocation = %v; want %v", args, wantArgs)
			}
		})
	}
}
