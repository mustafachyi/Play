package browser

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unsafe"
)

const nativeHostRegistryPath = `Software\Mozilla\NativeMessagingHosts\youtube_play`

var registryAPI = syscall.NewLazyDLL("advapi32.dll")

type windowsRegistration struct{ path string }

func describeRegistrationConflict(path string) string {
	return "Firefox registration points elsewhere:\n" + path
}

func formatInvalidRegistrationValueError(detail string) error {
	return fmt.Errorf("invalid Firefox registration: %s", detail)
}

func (registration windowsRegistration) read() (string, error) {
	path, err := syscall.UTF16PtrFromString(registration.path)
	if err != nil {
		return "", err
	}
	var key syscall.Handle
	err = syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, syscall.KEY_QUERY_VALUE, &key)
	if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer syscall.RegCloseKey(key)
	value, _, err := readRegistrationValue(key)
	return value, err
}

func readRegistrationValue(key syscall.Handle) (string, bool, error) {
	var kind, size uint32
	err := syscall.RegQueryValueEx(key, nil, nil, &kind, nil, &size)
	if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if kind != syscall.REG_SZ || size < 2 || size > 65536 || size%2 != 0 {
		return "", true, formatInvalidRegistrationValueError("The registry value must contain a full manifest file path.")
	}
	buffer := make([]uint16, size/2)
	if err := syscall.RegQueryValueEx(key, nil, nil, &kind, (*byte)(unsafe.Pointer(&buffer[0])), &size); err != nil {
		return "", true, err
	}
	if kind != syscall.REG_SZ || size < 2 || size%2 != 0 || size > uint32(len(buffer)*2) {
		return "", true, formatInvalidRegistrationValueError("The registry value changed or has an invalid size or type.")
	}
	buffer = buffer[:size/2]
	if buffer[len(buffer)-1] != 0 {
		return "", true, formatInvalidRegistrationValueError("The registry path is not null-terminated.")
	}
	for _, character := range buffer[:len(buffer)-1] {
		if character == 0 {
			return "", true, formatInvalidRegistrationValueError("The registry path contains a null character.")
		}
	}
	value := syscall.UTF16ToString(buffer)
	encodedPath, err := syscall.UTF16FromString(value)
	if err != nil || !filepath.IsAbs(value) || !slices.Equal(encodedPath, buffer) {
		return "", true, formatInvalidRegistrationValueError("The registry path is relative or has invalid text encoding.")
	}
	return value, true, nil
}

func (registration windowsRegistration) remove(expected string) error {
	if !strings.Contains(strings.Trim(registration.path, `\`), `\`) {
		return errors.New("invalid Firefox registry key")
	}
	if !filepath.IsAbs(expected) {
		return errors.New("invalid Firefox manifest path")
	}
	path, err := syscall.UTF16PtrFromString(registration.path)
	if err != nil {
		return err
	}
	var key syscall.Handle
	err = syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, syscall.KEY_QUERY_VALUE|syscall.KEY_SET_VALUE, &key)
	if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	}
	defer syscall.RegCloseKey(key)
	value, exists, err := readRegistrationValue(key)
	if err != nil {
		return err
	}
	if exists {
		if !strings.EqualFold(filepath.Clean(value), filepath.Clean(expected)) {
			return errors.New(describeRegistrationConflict(value))
		}
		status, _, _ := registryAPI.NewProc("RegDeleteValueW").Call(uintptr(key), 0)
		if status != 0 && syscall.Errno(status) != syscall.ERROR_FILE_NOT_FOUND {
			return syscall.Errno(status)
		}
	}
	var subkeys, values uint32
	if err := syscall.RegQueryInfoKey(key, nil, nil, nil, &subkeys, nil, nil, &values, nil, nil, nil, nil); err != nil {
		return err
	}
	if subkeys != 0 || values != 0 {
		return nil
	}
	return removeEmptyRegistrationKey(registration.path)
}

func (registration windowsRegistration) write(value string) error {
	if !filepath.IsAbs(value) {
		return errors.New("invalid Firefox manifest path")
	}
	path, err := syscall.UTF16PtrFromString(registration.path)
	if err != nil {
		return err
	}
	data, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}
	var key syscall.Handle
	var disposition uint32
	status, _, _ := registryAPI.NewProc("RegCreateKeyExW").Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(path)), 0, 0, 0, syscall.KEY_QUERY_VALUE|syscall.KEY_SET_VALUE, 0, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&disposition)))
	if status != 0 {
		return syscall.Errno(status)
	}
	current, exists, readError := readRegistrationValue(key)
	if readError != nil {
		syscall.RegCloseKey(key)
		return readError
	}
	if exists && !samePath(current, value) {
		syscall.RegCloseKey(key)
		return errors.New(describeRegistrationConflict(current))
	}
	status, _, _ = registryAPI.NewProc("RegSetValueExW").Call(uintptr(key), 0, 0, syscall.REG_SZ, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	syscall.RegCloseKey(key)
	if status == 0 {
		return nil
	}
	err = syscall.Errno(status)
	if disposition == 1 {
		if cleanup := removeEmptyRegistrationKey(registration.path); cleanup != nil {
			return errors.Join(err, fmt.Errorf("could not remove registry key: %w", cleanup))
		}
	}
	return err
}

func removeEmptyRegistrationKey(path string) error {
	keyPath, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	status, _, _ := syscall.NewLazyDLL("shlwapi.dll").NewProc("SHDeleteEmptyKeyW").Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(keyPath)))
	if status != 0 && syscall.Errno(status) != syscall.ERROR_FILE_NOT_FOUND {
		return syscall.Errno(status)
	}
	return nil
}
