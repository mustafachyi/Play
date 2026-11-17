package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memoryRegistration struct {
	value       string
	writeError  error
	removeError error
}

func (registration *memoryRegistration) read() (string, error) {
	return registration.value, nil
}

func (registration *memoryRegistration) write(value string) error {
	if registration.writeError != nil {
		return registration.writeError
	}
	if registration.value != "" && !samePath(registration.value, value) {
		return errors.New("unexpected owner")
	}
	registration.value = value
	return nil
}

func (registration *memoryRegistration) remove(expected string) error {
	if registration.removeError != nil {
		return registration.removeError
	}
	if registration.value != "" && !samePath(registration.value, expected) {
		return errors.New("unexpected owner")
	}
	registration.value = ""
	return nil
}

func lifecycleFixture(t *testing.T) (lifecycleManager, *memoryRegistration, string) {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "play.exe")
	mpvPath := filepath.Join(root, "mpv.exe")
	for _, path := range []string{executable, mpvPath} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	registration := &memoryRegistration{}
	manager := lifecycleManager{
		directory: filepath.Join(root, "Play", "BrowserIntegration"), executable: executable,
		registration: registration, writeFile: replaceIntegrationFile,
	}
	return manager, registration, mpvPath
}

func assertConnectionState(t *testing.T, manager lifecycleManager, expected string) connectionStatus {
	t.Helper()
	status, err := manager.inspect(true)
	if err != nil || status.State != expected {
		t.Fatalf("connection state: %+v, %v; expected %s", status, err, expected)
	}
	return status
}

func TestIntegrationOptInLifecycle(t *testing.T) {
	manager, registration, mpvPath := lifecycleFixture(t)
	assertConnectionState(t, manager, "disabled")
	if err := manager.refresh(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.directory); !errors.Is(err, os.ErrNotExist) || registration.value != "" {
		t.Fatalf("inspection or refresh enabled integration: %v, %s", err, registration.value)
	}
	if err := manager.repair(); err == nil {
		t.Fatal("repair enabled an absent connection")
	}
	for range 2 {
		if err := manager.enable(mpvPath, false); err != nil {
			t.Fatal(err)
		}
		status := assertConnectionState(t, manager, "enabled")
		if status.PlayPath != manager.executable || status.MPVPath != mpvPath || registration.value != manager.manifestPath() {
			t.Fatalf("unexpected integration ownership: %+v, %s", status, registration.value)
		}
		if err := manager.refresh(""); err != nil {
			t.Fatal(err)
		}
	}
	foreign := filepath.Join(manager.directory, "user-file.txt")
	if err := os.WriteFile(foreign, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := manager.disable(false); err != nil {
			t.Fatal(err)
		}
		assertConnectionState(t, manager, "disabled")
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "keep" {
		t.Fatalf("unrelated file changed: %s, %v", data, err)
	}
	if registration.value != "" {
		t.Fatal("disabled connection retained registration")
	}
	for _, path := range []string{manager.executable, mpvPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("application removed: %s: %v", path, err)
		}
	}
	if err := manager.enable(mpvPath, false); err != nil {
		t.Fatal(err)
	}
	assertConnectionState(t, manager, "enabled")
}

func TestIntegrationRequiresExplicitOwnerSwitch(t *testing.T) {
	first, registration, mpvPath := lifecycleFixture(t)
	if err := first.enable(mpvPath, false); err != nil {
		t.Fatal(err)
	}
	second := first
	second.executable = filepath.Join(filepath.Dir(first.executable), "other-play.exe")
	if err := os.WriteFile(second.executable, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	assertConnectionState(t, second, "conflict")
	if err := second.enable(mpvPath, false); err == nil {
		t.Fatal("different Play copy took over implicitly")
	}
	if err := second.refresh(""); err != nil {
		t.Fatal(err)
	}
	if err := second.disable(true); err != nil {
		t.Fatal(err)
	}
	assertConnectionState(t, first, "enabled")
	if err := second.enable("", true); err != nil {
		t.Fatal(err)
	}
	assertConnectionState(t, second, "enabled")
	if err := first.disable(false); err == nil {
		t.Fatal("previous owner disabled new owner")
	}
	if registration.value != second.manifestPath() {
		t.Fatal("owner switching changed the stable manifest location")
	}
}

func TestIntegrationPreservesForeignRegistration(t *testing.T) {
	manager, registration, mpvPath := lifecycleFixture(t)
	registration.value = filepath.Join(filepath.Dir(manager.directory), "foreign.json")
	expected := registration.value
	assertConnectionState(t, manager, "conflict")
	if err := manager.enable(mpvPath, true); err == nil {
		t.Fatal("explicit Play owner switch overwrote foreign registration")
	}
	if err := manager.disable(false); err == nil {
		t.Fatal("disable accepted foreign registration")
	}
	if err := manager.disable(true); err != nil {
		t.Fatal(err)
	}
	if registration.value != expected {
		t.Fatal("foreign registration changed")
	}
	if _, err := os.Stat(manager.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflict created metadata: %v", err)
	}
}

func TestIntegrationRepairAndMissingMPV(t *testing.T) {
	manager, registration, mpvPath := lifecycleFixture(t)
	if err := manager.enable(mpvPath, false); err != nil {
		t.Fatal(err)
	}
	registration.value = ""
	assertConnectionState(t, manager, "needs-attention")
	if err := manager.repair(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.manifestPath(), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	assertConnectionState(t, manager, "needs-attention")
	if err := manager.repair(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(mpvPath); err != nil {
		t.Fatal(err)
	}
	status := assertConnectionState(t, manager, "needs-attention")
	if status.MPVPath != mpvPath || !strings.Contains(status.Detail, "mpv") {
		t.Fatalf("missing dependency diagnosis: %+v", status)
	}
	ownerStatus, err := manager.inspect(false)
	if err != nil || ownerStatus.State != "enabled" {
		t.Fatalf("native ownership depends on mpv availability: %+v, %v", ownerStatus, err)
	}
	if err := manager.disable(false); err != nil {
		t.Fatal(err)
	}
	assertConnectionState(t, manager, "disabled")
}

func TestIntegrationManifestRequiresDescriptionString(t *testing.T) {
	for _, scenario := range []string{"missing", "null", "empty string", "custom string"} {
		t.Run(scenario, func(t *testing.T) {
			manager, _, mpvPath := lifecycleFixture(t)
			if err := manager.enable(mpvPath, false); err != nil {
				t.Fatal(err)
			}
			payload, err := os.ReadFile(manager.manifestPath())
			if err != nil {
				t.Fatal(err)
			}
			var manifest map[string]any
			if err := json.Unmarshal(payload, &manifest); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing":
				delete(manifest, "description")
			case "null":
				manifest["description"] = nil
			case "empty string":
				manifest["description"] = ""
			case "custom string":
				manifest["description"] = "Custom description"
			}
			payload, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manager.manifestPath(), payload, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "empty string" || scenario == "custom string" {
				assertConnectionState(t, manager, "enabled")
				return
			}
			assertConnectionState(t, manager, "needs-attention")
			if err := manager.repair(); err != nil {
				t.Fatal(err)
			}
			assertConnectionState(t, manager, "enabled")
		})
	}
}

func TestIntegrationManifestRequiresCanonicalFields(t *testing.T) {
	for _, field := range []string{"name", "description", "path", "type", "allowed_extensions"} {
		for _, variant := range []string{"renamed", "extra alias"} {
			t.Run(field+"/"+variant, func(t *testing.T) {
				manager, _, mpvPath := lifecycleFixture(t)
				if err := manager.enable(mpvPath, false); err != nil {
					t.Fatal(err)
				}
				payload, err := os.ReadFile(manager.manifestPath())
				if err != nil {
					t.Fatal(err)
				}
				var manifest map[string]any
				if err := json.Unmarshal(payload, &manifest); err != nil {
					t.Fatal(err)
				}
				alias := strings.ToUpper(field[:1]) + field[1:]
				manifest[alias] = manifest[field]
				if variant == "renamed" {
					delete(manifest, field)
				}
				payload, err = json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(manager.manifestPath(), payload, 0600); err != nil {
					t.Fatal(err)
				}
				assertConnectionState(t, manager, "needs-attention")
				if err := manager.repair(); err != nil {
					t.Fatal(err)
				}
				assertConnectionState(t, manager, "enabled")
			})
		}
	}
}

func TestIntegrationManifestRejectsFinalNullFields(t *testing.T) {
	for _, field := range []string{"name", "description", "path", "type", "allowed_extensions"} {
		for _, scenario := range []string{"initial null", "final null"} {
			t.Run(field+"/"+scenario, func(t *testing.T) {
				manager, _, mpvPath := lifecycleFixture(t)
				if err := manager.enable(mpvPath, false); err != nil {
					t.Fatal(err)
				}
				payload, err := os.ReadFile(manager.manifestPath())
				if err != nil {
					t.Fatal(err)
				}
				nullField := `"` + field + `":null`
				if scenario == "final null" {
					payload = []byte(strings.TrimSuffix(string(payload), "}") + "," + nullField + "}")
				} else {
					payload = []byte("{" + nullField + "," + strings.TrimPrefix(string(payload), "{"))
				}
				if err := os.WriteFile(manager.manifestPath(), payload, 0600); err != nil {
					t.Fatal(err)
				}
				if scenario == "initial null" {
					assertConnectionState(t, manager, "enabled")
					return
				}
				assertConnectionState(t, manager, "needs-attention")
				if err := manager.repair(); err != nil {
					t.Fatal(err)
				}
				assertConnectionState(t, manager, "enabled")
			})
		}
	}
}

func TestIntegrationInstallRollsBackOrdinaryFailures(t *testing.T) {
	for _, failure := range []string{"first file", "second file", "registration"} {
		t.Run(failure, func(t *testing.T) {
			manager, registration, mpvPath := lifecycleFixture(t)
			if err := manager.enable(mpvPath, false); err != nil {
				t.Fatal(err)
			}
			before := make(map[string][]byte)
			for _, path := range []string{manager.settingsPath(), manager.manifestPath()} {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = data
			}
			writes := 0
			manager.writeFile = func(path string, payload []byte) error {
				writes++
				if (failure == "first file" && writes == 1) || (failure == "second file" && writes == 2) {
					return errors.New("fixture write failure")
				}
				return replaceIntegrationFile(path, payload)
			}
			if failure == "registration" {
				registration.writeError = errors.New("fixture registration failure")
			}
			if err := manager.enable(mpvPath, false); err == nil {
				t.Fatal("installation failure reported success")
			}
			for path, expected := range before {
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("rollback mismatch for %s: %s, %v", path, actual, err)
				}
			}
			assertConnectionState(t, manager, "enabled")
		})
	}
	manager, registration, mpvPath := lifecycleFixture(t)
	registration.writeError = errors.New("fixture registration failure")
	if err := manager.enable(mpvPath, false); err == nil {
		t.Fatal("new connection failure reported success")
	}
	assertConnectionState(t, manager, "disabled")
}

func TestDisableFailureStillRevokesNativePlayback(t *testing.T) {
	manager, registration, mpvPath := lifecycleFixture(t)
	if err := manager.enable(mpvPath, false); err != nil {
		t.Fatal(err)
	}
	registration.removeError = errors.New("fixture removal failure")
	if err := manager.disable(false); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("incomplete removal diagnosis: %v", err)
	}
	status, err := manager.inspect(false)
	if err != nil || status.State == "enabled" {
		t.Fatalf("failed disable left native playback enabled: %+v, %v", status, err)
	}
	if err := manager.refresh(""); err != nil {
		t.Fatal(err)
	}
	settings, _, err := manager.readSettings()
	if err != nil || settings.Enabled {
		t.Fatalf("refresh overrode disabled intent: %+v, %v", settings, err)
	}
	registration.removeError = nil
	if err := manager.disable(false); err != nil {
		t.Fatal(err)
	}
	assertConnectionState(t, manager, "disabled")
}

func TestMetadataBoundariesRejectUnknownOrRedirectedOwnership(t *testing.T) {
	manager, _, mpvPath := lifecycleFixture(t)
	if err := os.MkdirAll(manager.directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.settingsPath(), []byte(`{"version":1,"unexpected":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.enable(mpvPath, false); err == nil {
		t.Fatal("malformed ownership settings overwritten")
	}
	if err := os.WriteFile(manager.settingsPath(), bytes.Repeat([]byte("x"), maxSettingsSize+1), 0600); err != nil {
		t.Fatal(err)
	}
	assertConnectionState(t, manager, "needs-attention")
	if err := os.Remove(manager.settingsPath()); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(filepath.Dir(manager.directory), "outside.json")
	if err := os.WriteFile(foreign, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, manager.settingsPath()); err != nil {
		t.Skipf("Windows symlink creation unavailable: %v", err)
	}
	assertConnectionState(t, manager, "conflict")
	if err := manager.enable(mpvPath, true); err == nil {
		t.Fatal("redirected metadata overwritten")
	}
	if payload, err := os.ReadFile(foreign); err != nil || string(payload) != "keep" {
		t.Fatalf("redirect target changed: %s, %v", payload, err)
	}
}

func TestManagementStatusAndCancelHaveNoSetupEffects(t *testing.T) {
	manager, registration, _ := lifecycleFixture(t)
	var output bytes.Buffer
	if err := runManagement(context.Background(), manager, []string{"status"}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "Firefox integration disabled.\n" {
		t.Fatalf("status output: %s", output.Bytes())
	}
	output.Reset()
	if err := runManagement(context.Background(), manager, nil, strings.NewReader("\n"), &output, &output); err != nil {
		t.Fatal(err)
	}
	if registration.value != "" {
		t.Fatal("cancel enabled Firefox integration")
	}
	if _, err := os.Stat(manager.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status or cancellation wrote metadata: %v", err)
	}
	for _, args := range [][]string{{"status", "extra"}, {"enable", "--bad"}, {"disable", "extra"}, {"unknown"}} {
		if err := runManagement(context.Background(), manager, args, strings.NewReader(""), &output, &output); err == nil {
			t.Fatalf("invalid management arguments accepted: %v", args)
		}
	}
}

func TestScoopExplicitVersionHandoverPreservesConsent(t *testing.T) {
	for _, scenario := range []string{"enabled", "disabled", "wrong previous", "different package", "saved current alias"} {
		t.Run(scenario, func(t *testing.T) {
			old, registration, mpvPath := lifecycleFixture(t)
			root := filepath.Dir(old.executable)
			old.executable = filepath.Join(root, "custom-scoop", "apps", "play", "0.1.4", "play.exe")
			updated := old
			updated.executable = filepath.Join(root, "custom-scoop", "apps", "play", "0.1.5", "play.exe")
			if scenario == "different package" {
				updated.executable = filepath.Join(root, "other-scoop", "apps", "play", "0.1.5", "play.exe")
			}
			for _, path := range []string{old.executable, updated.executable} {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			previousExecutable := old.executable
			if scenario == "saved current alias" {
				old.executable = filepath.Join(filepath.Dir(filepath.Dir(previousExecutable)), "current", "play.exe")
				if err := os.MkdirAll(filepath.Dir(old.executable), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(previousExecutable, old.executable); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "disabled" {
				if err := old.enable(mpvPath, false); err != nil {
					t.Fatal(err)
				}
			}
			previous := previousExecutable
			if scenario == "wrong previous" {
				previous = filepath.Join(filepath.Dir(old.executable), "unrelated.exe")
			}
			if err := updated.refresh(previous); err != nil {
				t.Fatal(err)
			}
			if scenario == "enabled" || scenario == "saved current alias" {
				assertConnectionState(t, updated, "enabled")
				if err := old.disable(true); err != nil {
					t.Fatal(err)
				}
				assertConnectionState(t, updated, "enabled")
			} else if scenario == "disabled" {
				assertConnectionState(t, updated, "disabled")
				if registration.value != "" {
					t.Fatal("Scoop update enabled browser integration")
				}
			} else {
				assertConnectionState(t, old, "enabled")
			}
		})
	}
}

func TestDisableRetriesEmptyFolderCleanup(t *testing.T) {
	manager, _, _ := lifecycleFixture(t)
	if err := os.MkdirAll(manager.directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := manager.disable(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry preserved empty integration folder: %v", err)
	}
}

func TestIntegrationCommitPreservesConcurrentForeignRegistration(t *testing.T) {
	manager, registration, mpvPath := lifecycleFixture(t)
	foreign := filepath.Join(filepath.Dir(manager.directory), "foreign.json")
	writes := 0
	manager.writeFile = func(path string, payload []byte) error {
		writes++
		if writes == 2 {
			registration.value = foreign
		}
		return replaceIntegrationFile(path, payload)
	}
	if err := manager.enable(mpvPath, false); err == nil {
		t.Fatal("concurrent registration change reported successful installation")
	}
	if registration.value != foreign {
		t.Fatal("concurrent foreign registration overwritten")
	}
	for _, path := range []string{manager.settingsPath(), manager.manifestPath()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial installation remained after ownership race: %s: %v", path, err)
		}
	}
}

func TestIntegrationMutexSerializesSetup(t *testing.T) {
	mutexName := "Local\\PlayBrowserSetupTest-" + filepath.Base(filepath.Dir(t.TempDir()))
	release, err := lockIntegrationMutex(mutexName)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		unlock, err := lockIntegrationMutex(mutexName)
		if err == nil {
			unlock()
		}
		result <- err
	}()
	err = <-result
	release()
	if err == nil || !strings.Contains(err.Error(), "setup is busy") {
		t.Fatalf("concurrent setup was not rejected: %v", err)
	}
	release, err = lockIntegrationMutex(mutexName)
	if err != nil {
		t.Fatalf("setup mutex was not released: %v", err)
	}
	release()
}

func TestUninstallCleansOwnedMetadataWithoutTouchingForeignConnection(t *testing.T) {
	manager, registration, mpvPath := lifecycleFixture(t)
	if err := manager.enable(mpvPath, false); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(filepath.Dir(manager.directory), "another-host.json")
	registration.value = foreign
	if err := manager.disable(true); err != nil {
		t.Fatal(err)
	}
	if registration.value != foreign {
		t.Fatal("uninstall changed another native connection")
	}
	for _, path := range []string{manager.settingsPath(), manager.manifestPath()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("uninstall retained owned metadata: %s: %v", path, err)
		}
	}
}
