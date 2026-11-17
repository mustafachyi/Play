package browser

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/user"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

func Manage(ctx context.Context, args []string, input io.Reader, output, errorOutput io.Writer) error {
	manager, err := currentManager()
	if err != nil {
		return err
	}
	return runManagement(ctx, manager, args, input, output, errorOutput)
}

func runManagement(ctx context.Context, manager lifecycleManager, args []string, input io.Reader, output, errorOutput io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) == 0 {
		return promptManagement(ctx, manager, input, output, errorOutput)
	}
	action := args[0]
	if action == "status" {
		if len(args) != 1 {
			return errors.New("usage: play -browser status")
		}
		status, err := manager.inspect(true)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, status.Detail)
		return err
	}
	if action == "help" || action == "-help" || action == "-h" {
		if len(args) != 1 {
			return errors.New("usage: play -browser help")
		}
		_, err := io.WriteString(output, "Firefox integration\n\nplay -browser                       Open setup\nplay -browser enable [mpv.exe path] Enable\nplay -browser enable --switch       Use this Play installation\nplay -browser status                Show status\nplay -browser repair                Repair\nplay -browser disable               Disable\n")
		return err
	}
	if action != "enable" && action != "repair" && action != "disable" && action != "refresh" && action != "remove" {
		return errors.New("unknown command; run play -browser help")
	}
	var mpvPath string
	var switchOwner bool
	if action == "enable" {
		for _, argument := range args[1:] {
			if argument == "--switch" && !switchOwner {
				switchOwner = true
				continue
			}
			if mpvPath != "" || strings.HasPrefix(argument, "-") {
				return errors.New("usage: play -browser enable [--switch] [absolute mpv.exe path]")
			}
			mpvPath = argument
		}
	} else if len(args) != 1 && !(action == "refresh" && len(args) == 2) {
		return fmt.Errorf("usage: play -browser %s", action)
	}
	release, err := lockIntegrationManagement()
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	switch action {
	case "enable":
		err = manager.enable(mpvPath, switchOwner)
	case "repair":
		err = manager.repair()
	case "disable":
		err = manager.disable(false)
	case "refresh":
		previousExecutable := ""
		if len(args) == 2 {
			previousExecutable = args[1]
		}
		err = manager.refresh(previousExecutable)
	case "remove":
		err = manager.disable(true)
	}
	if err != nil {
		return err
	}
	if action == "refresh" || action == "remove" {
		return nil
	}
	status, err := manager.inspect(true)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, status.Detail)
	return err
}

func promptManagement(ctx context.Context, manager lifecycleManager, input io.Reader, output, errorOutput io.Writer) error {
	if closer, ok := input.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	status, err := manager.inspect(true)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "%s\n\n1. Enable\n2. Status\n3. Disable\n4. Repair\n\nSelect an option (Enter to cancel): ", status.Detail); err != nil {
		return err
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 1024), maxSettingsSize)
	scanned, err := scanManagementLine(ctx, scanner)
	if err != nil || !scanned {
		return err
	}
	choice := strings.TrimSpace(scanner.Text())
	if choice == "" {
		return nil
	}
	var args []string
	switch choice {
	case "1":
		if status.State == "conflict" {
			if status.PlayPath != "" {
				return errors.New("run play -browser enable --switch to use this installation")
			}
			return errors.New("resolve the reported conflict before enabling integration")
		}
		args = []string{"enable"}
		prompt := "mpv.exe path (Enter to use PATH): "
		if status.MPVPath != "" {
			prompt = "mpv.exe path (Enter to keep current): "
		}
		if _, err := io.WriteString(output, prompt); err != nil {
			return err
		}
		scanned, err = scanManagementLine(ctx, scanner)
		if err != nil || !scanned {
			return err
		}
		if path := strings.Trim(strings.TrimSpace(scanner.Text()), "\""); path != "" {
			args = append(args, path)
		}
	case "2":
		args = []string{"status"}
	case "3":
		args = []string{"disable"}
	case "4":
		args = []string{"repair"}
	default:
		return errors.New("choose 1, 2, 3, 4, or Enter to cancel")
	}
	return runManagement(ctx, manager, args, input, output, errorOutput)
}

func scanManagementLine(ctx context.Context, scanner *bufio.Scanner) (bool, error) {
	scanned := scanner.Scan()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return scanned, scanner.Err()
}

func lockIntegrationManagement() (func(), error) {
	identity, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("identify the current Windows user: %w", err)
	}
	return lockIntegrationMutex("Local\\PlayFirefoxIntegration-" + identity.Uid)
}

func lockIntegrationMutex(mutexName string) (func(), error) {
	name, err := syscall.UTF16PtrFromString(mutexName)
	if err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	handle, _, callError := lifecycleKernelAPI.NewProc("CreateMutexW").Call(0, 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		runtime.UnlockOSThread()
		return nil, callError
	}
	result, _, waitError := lifecycleKernelAPI.NewProc("WaitForSingleObject").Call(handle, 0)
	if result != 0 && result != 0x80 {
		syscall.CloseHandle(syscall.Handle(handle))
		runtime.UnlockOSThread()
		if result == 0x102 {
			return nil, errors.New("Firefox setup is busy; try again when it finishes")
		}
		return nil, waitError
	}
	return func() {
		lifecycleKernelAPI.NewProc("ReleaseMutex").Call(handle)
		syscall.CloseHandle(syscall.Handle(handle))
		runtime.UnlockOSThread()
	}, nil
}
