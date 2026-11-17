package browser

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func registrationFixture(t *testing.T) windowsRegistration {
	t.Helper()
	root := t.TempDir()
	registration := windowsRegistration{path: `Software\PlayBrowserRemovalTest-` + filepath.Base(filepath.Dir(root)) + "-" + filepath.Base(root)}
	path, err := syscall.UTF16PtrFromString(registration.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		status, _, _ := syscall.NewLazyDLL("shlwapi.dll").NewProc("SHDeleteKeyW").Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(path)))
		if status != 0 && syscall.Errno(status) != syscall.ERROR_FILE_NOT_FOUND {
			t.Errorf("fixture cleanup failed: %d", status)
		}
	})
	return registration
}

func openRegistrationFixture(t *testing.T, registration windowsRegistration) syscall.Handle {
	t.Helper()
	path, err := syscall.UTF16PtrFromString(registration.path)
	if err != nil {
		t.Fatal(err)
	}
	var key syscall.Handle
	status, _, _ := registryAPI.NewProc("RegCreateKeyExW").Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(path)), 0, 0, 0, syscall.KEY_ALL_ACCESS, 0, uintptr(unsafe.Pointer(&key)), 0)
	if status != 0 {
		t.Fatal(syscall.Errno(status))
	}
	t.Cleanup(func() { syscall.RegCloseKey(key) })
	return key
}

func setRegistrationFixtureValue(t *testing.T, key syscall.Handle, name string, kind uint32, payload []byte) {
	t.Helper()
	value, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	var data *byte
	if len(payload) > 0 {
		data = &payload[0]
	}
	status, _, _ := registryAPI.NewProc("RegSetValueExW").Call(uintptr(key), uintptr(unsafe.Pointer(value)), 0, uintptr(kind), uintptr(unsafe.Pointer(data)), uintptr(len(payload)))
	if status != 0 {
		t.Fatal(syscall.Errno(status))
	}
}

func assertRegistrationKeyAbsent(t *testing.T, registration windowsRegistration) {
	t.Helper()
	path, _ := syscall.UTF16PtrFromString(registration.path)
	var key syscall.Handle
	err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, syscall.KEY_QUERY_VALUE, &key)
	if err == nil {
		syscall.RegCloseKey(key)
		t.Fatal("empty registration key was preserved")
	}
	if !errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
		t.Fatal(err)
	}
}

func TestRemoveWindowsRegistration(t *testing.T) {
	expected := `C:\Users\Example\Companion\youtube_play.json`
	t.Run("missing", func(t *testing.T) {
		registration := registrationFixture(t)
		if err := registration.remove(expected); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("matching", func(t *testing.T) {
		registration := registrationFixture(t)
		if err := registration.write(strings.ToUpper(expected)); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := registration.remove(filepath.Dir(expected) + `\.\` + filepath.Base(expected)); err != nil {
				t.Fatal(err)
			}
		}
		assertRegistrationKeyAbsent(t, registration)
	})
	t.Run("foreign", func(t *testing.T) {
		registration := registrationFixture(t)
		foreign := `C:\Different\youtube_play.json`
		if err := registration.write(foreign); err != nil {
			t.Fatal(err)
		}
		if err := registration.remove(expected); err == nil {
			t.Fatal("foreign registration removed")
		}
		if actual, err := registration.read(); err != nil || actual != foreign {
			t.Fatalf("foreign registration changed: %q, %v", actual, err)
		}
	})
	t.Run("empty key", func(t *testing.T) {
		registration := registrationFixture(t)
		openRegistrationFixture(t, registration)
		if err := registration.remove(expected); err != nil {
			t.Fatal(err)
		}
		assertRegistrationKeyAbsent(t, registration)
	})
	for _, fixture := range []struct {
		name         string
		defaultValue bool
	}{{"without default", false}, {"with default", true}} {
		t.Run("preserve other values "+fixture.name, func(t *testing.T) {
			registration := registrationFixture(t)
			if fixture.defaultValue {
				if err := registration.write(expected); err != nil {
					t.Fatal(err)
				}
			}
			key := openRegistrationFixture(t, registration)
			setRegistrationFixtureValue(t, key, "foreign", syscall.REG_BINARY, []byte{1, 2, 3})
			if err := registration.remove(expected); err != nil {
				t.Fatal(err)
			}
			name, _ := syscall.UTF16PtrFromString("foreign")
			var kind uint32
			payload := make([]byte, 3)
			size := uint32(len(payload))
			if err := syscall.RegQueryValueEx(key, name, nil, &kind, &payload[0], &size); err != nil || kind != syscall.REG_BINARY || size != 3 || !bytes.Equal(payload, []byte{1, 2, 3}) {
				t.Fatalf("foreign value changed: %d, %v, %v", kind, payload, err)
			}
			if actual, err := registration.read(); err != nil || actual != "" {
				t.Fatalf("default still registered: %q, %v", actual, err)
			}
		})
	}
	t.Run("preserve subkeys", func(t *testing.T) {
		registration := registrationFixture(t)
		if err := registration.write(expected); err != nil {
			t.Fatal(err)
		}
		child := windowsRegistration{path: registration.path + `\foreign`}
		openRegistrationFixture(t, child)
		if err := registration.remove(expected); err != nil {
			t.Fatal(err)
		}
		path, _ := syscall.UTF16PtrFromString(child.path)
		var key syscall.Handle
		if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, syscall.KEY_QUERY_VALUE, &key); err != nil {
			t.Fatal(err)
		}
		syscall.RegCloseKey(key)
	})
}

func TestRemoveMalformedWindowsRegistration(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		kind    uint32
		payload []byte
	}{
		{"wrong type", syscall.REG_BINARY, []byte{1, 2}},
		{"odd size", syscall.REG_SZ, []byte{1}},
		{"empty bytes", syscall.REG_SZ, nil},
		{"empty string", syscall.REG_SZ, []byte{0, 0}},
		{"missing terminator", syscall.REG_SZ, []byte{'C', 0, ':', 0, '\\', 0, 'x', 0}},
		{"embedded terminator", syscall.REG_SZ, []byte{'C', 0, ':', 0, '\\', 0, 'x', 0, 0, 0, 'y', 0, 0, 0}},
		{"unpaired surrogate", syscall.REG_SZ, []byte{'C', 0, ':', 0, '\\', 0, 0, 0xd8, 0, 0}},
		{"oversized", syscall.REG_SZ, make([]byte, 65538)},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			registration := registrationFixture(t)
			key := openRegistrationFixture(t, registration)
			setRegistrationFixtureValue(t, key, "", fixture.kind, fixture.payload)
			if err := registration.remove(`C:\Companion\youtube_play.json`); err == nil {
				t.Fatal("malformed registration accepted")
			}
			var kind uint32
			payload := make([]byte, len(fixture.payload)+1)
			size := uint32(len(payload))
			if err := syscall.RegQueryValueEx(key, nil, nil, &kind, &payload[0], &size); err != nil || kind != fixture.kind || size != uint32(len(fixture.payload)) || !bytes.Equal(payload[:size], fixture.payload) {
				t.Fatalf("malformed registration changed: %d, %d, %v", kind, size, err)
			}
		})
	}
}

func TestRemoveWindowsRegistrationRequiresNestedKeyAndAbsolutePath(t *testing.T) {
	for _, path := range []string{"", `\`, "Software", `Software\`} {
		if err := (windowsRegistration{path: path}).remove(`C:\Companion\youtube_play.json`); err == nil {
			t.Fatalf("unsafe registry key accepted: %q", path)
		}
	}
	registration := registrationFixture(t)
	if err := registration.remove("youtube_play.json"); err == nil {
		t.Fatal("relative expected registration accepted")
	}
}

func TestEmptyRegistrationCleanupPreservesForeignValues(t *testing.T) {
	registration := registrationFixture(t)
	key := openRegistrationFixture(t, registration)
	setRegistrationFixtureValue(t, key, "foreign", syscall.REG_BINARY, []byte{1, 2, 3})
	if err := removeEmptyRegistrationKey(registration.path); err == nil {
		t.Fatal("nonempty registration key accepted for cleanup")
	}
	if _, err := registration.read(); err != nil {
		t.Fatalf("registration key was removed: %v", err)
	}
	name, _ := syscall.UTF16PtrFromString("foreign")
	var kind uint32
	payload := make([]byte, 3)
	size := uint32(len(payload))
	if err := syscall.RegQueryValueEx(key, name, nil, &kind, &payload[0], &size); err != nil || kind != syscall.REG_BINARY || size != 3 || !bytes.Equal(payload, []byte{1, 2, 3}) {
		t.Fatalf("foreign value changed during cleanup: %d, %v, %v", kind, payload, err)
	}
}

func TestWindowsRegistration(t *testing.T) {
	registration := windowsRegistration{path: `Software\PlayBrowserRegistryTest-` + filepath.Base(filepath.Dir(t.TempDir()))}
	path, _ := syscall.UTF16PtrFromString(registration.path)
	t.Cleanup(func() {
		status, _, _ := registryAPI.NewProc("RegDeleteKeyW").Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(path)))
		if status != 0 && syscall.Errno(status) != syscall.ERROR_FILE_NOT_FOUND {
			t.Errorf("fixture registry cleanup failed: %d", status)
		}
	})
	if actual, err := registration.read(); err != nil || actual != "" {
		t.Fatalf("missing registration: %s, %v", actual, err)
	}
	for _, value := range []string{`C:\Users\Example\youtube_play.json`, `C:\Users\日本語\youtube_play.json`} {
		if err := registration.write(value); err != nil {
			t.Fatal(err)
		}
		if actual, err := registration.read(); err != nil || actual != value {
			t.Fatalf("registration round trip: %s, %v", actual, err)
		}
		if err := registration.remove(value); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWindowsRegistrationWritePreservesForeignDefault(t *testing.T) {
	registration := registrationFixture(t)
	foreign := `C:\Different\youtube_play.json`
	if err := registration.write(foreign); err != nil {
		t.Fatal(err)
	}
	key := openRegistrationFixture(t, registration)
	setRegistrationFixtureValue(t, key, "unrelated", syscall.REG_BINARY, []byte{7, 8})
	if err := registration.write(`C:\Play\BrowserIntegration\youtube_play.json`); err == nil {
		t.Fatal("write replaced an existing foreign default")
	}
	if actual, err := registration.read(); err != nil || actual != foreign {
		t.Fatalf("foreign default changed: %q, %v", actual, err)
	}
	name, _ := syscall.UTF16PtrFromString("unrelated")
	var kind uint32
	payload := make([]byte, 2)
	size := uint32(len(payload))
	if err := syscall.RegQueryValueEx(key, name, nil, &kind, &payload[0], &size); err != nil || kind != syscall.REG_BINARY || !bytes.Equal(payload, []byte{7, 8}) {
		t.Fatalf("unrelated named value changed: %v, %v", payload, err)
	}
	malformed := []byte{1, 2, 3}
	setRegistrationFixtureValue(t, key, "", syscall.REG_BINARY, malformed)
	if err := registration.write(`C:\Play\BrowserIntegration\youtube_play.json`); err == nil {
		t.Fatal("write replaced a malformed existing default")
	}
	payload = make([]byte, len(malformed))
	size = uint32(len(payload))
	if err := syscall.RegQueryValueEx(key, nil, nil, &kind, &payload[0], &size); err != nil || kind != syscall.REG_BINARY || !bytes.Equal(payload, malformed) {
		t.Fatalf("malformed default changed: %v, %v", payload, err)
	}
}
