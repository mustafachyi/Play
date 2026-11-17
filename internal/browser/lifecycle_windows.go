package browser

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const nativeHostName = "youtube_play"
const extensionID = "youtube-play@mustafachyi.dev"
const maxSettingsSize = 16384

var lifecycleKernelAPI = syscall.NewLazyDLL("kernel32.dll")

type connectionStatus struct {
	State    string `json:"state"`
	PlayPath string `json:"playPath,omitempty"`
	MPVPath  string `json:"mpvPath,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type browserSettings struct {
	Version  int    `json:"version"`
	Enabled  bool   `json:"enabled"`
	PlayPath string `json:"playPath"`
	MPVPath  string `json:"mpvPath"`
}

type nativeHostManifest struct {
	Name              string   `json:"name"`
	Description       *string  `json:"description"`
	Path              string   `json:"path"`
	Type              string   `json:"type"`
	AllowedExtensions []string `json:"allowed_extensions"`
}

type registrationStore interface {
	read() (string, error)
	write(string) error
	remove(string) error
}

type lifecycleManager struct {
	directory    string
	executable   string
	registration registrationStore
	writeFile    func(string, []byte) error
}

type fileSnapshot struct {
	name    string
	payload []byte
	exists  bool
}

func nativeManifestPath() (string, error) {
	directory, err := integrationDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "youtube_play.json"), nil
}

func validateNativeOwner() (connectionStatus, error) {
	manager, err := currentManager()
	if err != nil {
		return connectionStatus{}, err
	}
	return manager.inspect(false)
}

func integrationDirectory() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if !filepath.IsAbs(base) {
		return "", errors.New("LOCALAPPDATA must contain an absolute folder path")
	}
	return filepath.Join(base, "Play", "BrowserIntegration"), nil
}

func currentManager() (lifecycleManager, error) {
	directory, err := integrationDirectory()
	if err != nil {
		return lifecycleManager{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return lifecycleManager{}, fmt.Errorf("locate Play: %w", err)
	}
	return lifecycleManager{
		directory: directory, executable: stableExecutablePath(executable),
		registration: windowsRegistration{path: nativeHostRegistryPath}, writeFile: replaceIntegrationFile,
	}, nil
}

func stableExecutablePath(executable string) string {
	executable = filepath.Clean(executable)
	installation := filepath.Dir(filepath.Dir(executable))
	if !strings.EqualFold(filepath.Base(installation), "play") || !strings.EqualFold(filepath.Base(filepath.Dir(installation)), "apps") {
		return executable
	}
	candidate := filepath.Join(installation, "current", filepath.Base(executable))
	if sameExecutablePath(candidate, executable) {
		return candidate
	}
	return executable
}

func sameExecutablePath(first, second string) bool {
	if strings.EqualFold(filepath.Clean(first), filepath.Clean(second)) {
		return true
	}
	firstInfo, firstError := os.Stat(first)
	secondInfo, secondError := os.Stat(second)
	return firstError == nil && secondError == nil && os.SameFile(firstInfo, secondInfo)
}

func (manager lifecycleManager) manifestPath() string {
	return filepath.Join(manager.directory, "youtube_play.json")
}

func (manager lifecycleManager) settingsPath() string {
	return filepath.Join(manager.directory, "settings.json")
}

func (manager lifecycleManager) inspect(checkMPV bool) (connectionStatus, error) {
	status := connectionStatus{State: "disabled", Detail: "Firefox integration disabled."}
	if err := validateMetadataDirectory(manager.directory); err != nil {
		status.State, status.Detail = "conflict", err.Error()
		return status, nil
	}
	registered, err := manager.registration.read()
	if err != nil {
		return status, fmt.Errorf("read Firefox connection: %w", err)
	}
	if registered != "" && !samePath(registered, manager.manifestPath()) {
		status.State, status.Detail = "conflict", describeRegistrationConflict(registered)
		return status, nil
	}
	settings, exists, err := manager.readSettings()
	if err != nil {
		status.State, status.Detail = "needs-attention", err.Error()
		return status, nil
	}
	if !exists {
		_, manifestError := os.Stat(manager.manifestPath())
		if registered != "" || manifestError == nil {
			status.State, status.Detail = "needs-attention", "Integration settings missing: "+manager.settingsPath()
		}
		return status, nil
	}
	status.PlayPath, status.MPVPath = settings.PlayPath, settings.MPVPath
	if !sameExecutablePath(settings.PlayPath, manager.executable) {
		status.State, status.Detail = "conflict", "Another Play installation owns Firefox integration: "+settings.PlayPath
		return status, nil
	}
	if !settings.Enabled {
		if registered != "" {
			status.State, status.Detail = "needs-attention", "Firefox cleanup incomplete. Run play -browser disable."
		}
		return status, nil
	}
	status.State = "needs-attention"
	if registered == "" {
		status.Detail = "Firefox integration needs repair. Run play -browser repair."
		return status, nil
	}
	if err := manager.validateManifest(settings); err != nil {
		status.Detail = err.Error()
		return status, nil
	}
	if err := validateApplicationPath(settings.PlayPath, false); err != nil {
		status.Detail = fmt.Sprintf("Cannot access Play: %v", err)
		return status, nil
	}
	if checkMPV {
		if err := validateApplicationPath(settings.MPVPath, true); err != nil {
			status.Detail = fmt.Sprintf("Cannot access mpv: %v. Run play -browser to select mpv.exe.", err)
			return status, nil
		}
	}
	status.State, status.Detail = "enabled", "Firefox integration enabled."
	return status, nil
}

func (manager lifecycleManager) readSettings() (browserSettings, bool, error) {
	var settings browserSettings
	payload, exists, err := readIntegrationFile(manager.settingsPath())
	if err != nil || !exists {
		return settings, exists, err
	}
	if err := decodeIntegrationDocument(payload, &settings); err != nil {
		return settings, true, fmt.Errorf("invalid integration settings (%s): %w", manager.settingsPath(), err)
	}
	if settings.Version != 1 || !filepath.IsAbs(settings.PlayPath) || !filepath.IsAbs(settings.MPVPath) || !strings.EqualFold(filepath.Base(settings.MPVPath), "mpv.exe") {
		return settings, true, fmt.Errorf("invalid integration settings: %s", manager.settingsPath())
	}
	return settings, true, nil
}

func (manager lifecycleManager) validateManifest(settings browserSettings) error {
	payload, exists, err := readIntegrationFile(manager.manifestPath())
	if err != nil {
		return fmt.Errorf("read Firefox manifest: %w", err)
	}
	if !exists {
		return errors.New("Firefox manifest missing; run play -browser repair")
	}
	var manifest nativeHostManifest
	if decodeNativeManifest(payload, &manifest) != nil || manifest.Name != nativeHostName || manifest.Description == nil || manifest.Type != "stdio" || !samePath(manifest.Path, settings.PlayPath) || len(manifest.AllowedExtensions) != 1 || manifest.AllowedExtensions[0] != extensionID {
		return errors.New("Firefox manifest mismatch; run play -browser repair")
	}
	return nil
}

func decodeNativeManifest(payload []byte, manifest *nativeHostManifest) error {
	var fields map[string]json.RawMessage
	if err := decodeIntegrationDocument(payload, &fields); err != nil {
		return err
	}
	required := [...]string{"name", "description", "path", "type", "allowed_extensions"}
	if len(fields) != len(required) {
		return errors.New("invalid Firefox manifest fields")
	}
	for _, field := range required {
		if value, exists := fields[field]; !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("invalid Firefox manifest fields")
		}
	}
	return decodeIntegrationDocument(payload, manifest)
}

func (manager lifecycleManager) enable(mpvPath string, switchOwner bool) error {
	if err := validateMetadataDirectory(manager.directory); err != nil {
		return err
	}
	registered, err := manager.registration.read()
	if err != nil {
		return err
	}
	if registered != "" && !samePath(registered, manager.manifestPath()) {
		return errors.New(describeRegistrationConflict(registered))
	}
	previous, exists, err := manager.readSettings()
	if err != nil {
		return err
	}
	if exists && !sameExecutablePath(previous.PlayPath, manager.executable) && !switchOwner {
		return errors.New("another Play installation owns Firefox integration; use play -browser enable --switch")
	}
	if !exists {
		if _, exists, err := readIntegrationFile(manager.manifestPath()); err != nil {
			return err
		} else if exists || registered != "" {
			return fmt.Errorf("integration settings missing: %s", manager.settingsPath())
		}
	}
	if err := validateApplicationPath(manager.executable, false); err != nil {
		return fmt.Errorf("validate Play: %w", err)
	}
	if mpvPath == "" && exists {
		mpvPath = previous.MPVPath
	}
	if mpvPath == "" {
		mpvPath, err = exec.LookPath("mpv.exe")
		if err != nil {
			return errors.New("mpv not found in PATH; run play -browser to select mpv.exe")
		}
	}
	if err := validateApplicationPath(mpvPath, true); err != nil {
		return fmt.Errorf("validate mpv: %w", err)
	}
	settings := browserSettings{Version: 1, Enabled: true, PlayPath: manager.executable, MPVPath: filepath.Clean(mpvPath)}
	return manager.install(settings)
}

func (manager lifecycleManager) install(settings browserSettings) error {
	description := "Connect YouTube Play in Firefox to Play"
	manifest := nativeHostManifest{
		Name: nativeHostName, Description: &description, Path: settings.PlayPath,
		Type: "stdio", AllowedExtensions: []string{extensionID},
	}
	settingsPayload, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	manifestPayload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if len(settingsPayload) > maxSettingsSize || len(manifestPayload) > maxSettingsSize {
		return errors.New("integration paths exceed the supported settings size")
	}
	return manager.commitFiles(map[string][]byte{"settings.json": settingsPayload, "youtube_play.json": manifestPayload})
}

func (manager lifecycleManager) repair() error {
	settings, exists, err := manager.readSettings()
	if err != nil {
		return err
	}
	if !exists || !settings.Enabled {
		return errors.New("Firefox integration disabled; run play -browser enable")
	}
	return manager.enable(settings.MPVPath, false)
}

func (manager lifecycleManager) refresh(previousExecutable string) error {
	if err := validateMetadataDirectory(manager.directory); err != nil {
		return err
	}
	settings, exists, err := manager.readSettings()
	if err != nil {
		return err
	}
	if !exists || !settings.Enabled {
		return nil
	}
	if !sameExecutablePath(settings.PlayPath, manager.executable) {
		if !filepath.IsAbs(previousExecutable) || !sameExecutablePath(settings.PlayPath, previousExecutable) || !sameScoopInstallation(previousExecutable, manager.executable) {
			return nil
		}
		return manager.enable(settings.MPVPath, true)
	}
	return manager.enable(settings.MPVPath, false)
}

func sameScoopInstallation(previous, current string) bool {
	previousRoot := filepath.Dir(filepath.Dir(previous))
	currentRoot := filepath.Dir(filepath.Dir(current))
	return samePath(previousRoot, currentRoot) && strings.EqualFold(filepath.Base(previousRoot), "play") &&
		strings.EqualFold(filepath.Base(filepath.Dir(previousRoot)), "apps") && strings.EqualFold(filepath.Base(previous), filepath.Base(current))
}

func (manager lifecycleManager) disable(onlyOwned bool) error {
	if err := validateMetadataDirectory(manager.directory); err != nil {
		return err
	}
	settings, exists, err := manager.readSettings()
	if err != nil {
		return err
	}
	registered, err := manager.registration.read()
	if err != nil {
		return err
	}
	foreignRegistration := registered != "" && !samePath(registered, manager.manifestPath())
	if foreignRegistration && !onlyOwned {
		return errors.New(describeRegistrationConflict(registered))
	}
	if !exists {
		if foreignRegistration {
			return nil
		}
		if registered != "" {
			return fmt.Errorf("integration settings missing: %s", manager.settingsPath())
		}
		return removeEmptyMetadataDirectory(manager.directory)
	}
	if !sameExecutablePath(settings.PlayPath, manager.executable) {
		if onlyOwned {
			return nil
		}
		return fmt.Errorf("Firefox integration belongs to %s; disable it from that installation", settings.PlayPath)
	}
	settings.Enabled = false
	payload, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := manager.writeFile(manager.settingsPath(), payload); err != nil {
		return fmt.Errorf("record disabled integration: %w", err)
	}
	if !foreignRegistration {
		if err := manager.registration.remove(manager.manifestPath()); err != nil {
			return fmt.Errorf("Firefox integration disabled; cleanup failed: %w", err)
		}
	}
	for _, name := range []string{"youtube_play.json", "settings.json"} {
		if err := os.Remove(filepath.Join(manager.directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("Firefox integration disabled; cleanup failed: %w", err)
		}
	}
	return removeEmptyMetadataDirectory(manager.directory)
}

func (manager lifecycleManager) commitFiles(payloads map[string][]byte) error {
	if err := validateMetadataDirectory(manager.directory); err != nil {
		return err
	}
	previousRegistration, err := manager.registration.read()
	if err != nil {
		return err
	}
	if previousRegistration != "" && !samePath(previousRegistration, manager.manifestPath()) {
		return errors.New(describeRegistrationConflict(previousRegistration))
	}
	snapshots := make([]fileSnapshot, 0, len(payloads))
	for _, name := range []string{"settings.json", "youtube_play.json"} {
		payload, exists, err := readIntegrationFile(filepath.Join(manager.directory, name))
		if err != nil {
			return err
		}
		snapshots = append(snapshots, fileSnapshot{name: name, payload: payload, exists: exists})
	}
	if err := os.MkdirAll(manager.directory, 0700); err != nil {
		return err
	}
	if err := validateMetadataDirectory(manager.directory); err != nil {
		return err
	}
	rollback := func(cause error) error {
		for _, snapshot := range snapshots {
			path := filepath.Join(manager.directory, snapshot.name)
			var err error
			if snapshot.exists {
				err = manager.writeFile(path, snapshot.payload)
			} else {
				err = os.Remove(path)
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				}
			}
			if err != nil {
				cause = errors.Join(cause, fmt.Errorf("could not restore %s: %w", snapshot.name, err))
			}
		}
		if err := removeEmptyMetadataDirectory(manager.directory); err != nil {
			cause = errors.Join(cause, fmt.Errorf("could not restore integration folder: %w", err))
		}
		return cause
	}
	for _, snapshot := range snapshots {
		if err := manager.writeFile(filepath.Join(manager.directory, snapshot.name), payloads[snapshot.name]); err != nil {
			return rollback(fmt.Errorf("write %s: %w", snapshot.name, err))
		}
	}
	if err := manager.registration.write(manager.manifestPath()); err != nil {
		return rollback(fmt.Errorf("register Firefox connection: %w", err))
	}
	return nil
}

func readIntegrationFile(path string) ([]byte, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, maxSettingsSize+1))
	if err != nil {
		return nil, true, err
	}
	if len(payload) > maxSettingsSize {
		return nil, true, errors.New("integration file exceeds its size limit")
	}
	return payload, true, nil
}

func decodeIntegrationDocument(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected data after integration document")
	}
	return nil
}

func validateApplicationPath(path string, mpv bool) error {
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Ext(path), ".exe") {
		return errors.New("select an absolute executable path")
	}
	if mpv && !strings.EqualFold(filepath.Base(path), "mpv.exe") {
		return errors.New("select mpv.exe")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("expected an executable file")
	}
	return nil
}

func samePath(first, second string) bool {
	return strings.EqualFold(filepath.Clean(first), filepath.Clean(second))
}

func isMetadataRedirect(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && attributes.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func validateMetadataDirectory(directory string) error {
	if !filepath.IsAbs(directory) || filepath.Dir(filepath.Clean(directory)) == filepath.Clean(directory) {
		return errors.New("invalid browser integration folder")
	}
	for path := filepath.Clean(directory); ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (isMetadataRedirect(info) || !info.IsDir()) {
			return fmt.Errorf("integration folder is redirected or blocked: %s", path)
		}
		if filepath.Dir(path) == path {
			break
		}
	}
	for _, name := range []string{"settings.json", "youtube_play.json"} {
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && (isMetadataRedirect(info) || !info.Mode().IsRegular()) {
			return fmt.Errorf("integration file is redirected or invalid: %s", path)
		}
	}
	return nil
}

func replaceIntegrationFile(path string, payload []byte) (result error) {
	stage, err := os.CreateTemp(filepath.Dir(path), ".play-browser-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(stage.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}()
	_, writeError := stage.Write(payload)
	syncError := stage.Sync()
	closeError := stage.Close()
	if err := errors.Join(writeError, syncError, closeError); err != nil {
		return err
	}
	from, err := syscall.UTF16PtrFromString(stage.Name())
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	success, _, callError := lifecycleKernelAPI.NewProc("MoveFileExW").Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), 0x1|0x8)
	if success == 0 {
		return callError
	}
	return nil
}

func removeEmptyMetadataDirectory(directory string) error {
	for _, path := range []string{directory, filepath.Dir(directory)} {
		entries, err := os.ReadDir(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			break
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
