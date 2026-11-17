package browser

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const nativeMessageLimit = 4096
const nativeDiagnosticLimit = 2048
const nativeStartupTimeout = 120 * time.Second

var nativePlaybackURL = regexp.MustCompile(`^https://www\.youtube\.com/(watch\?v=[A-Za-z0-9_-]{11}(&list=[A-Za-z0-9_-]{2,200})?|playlist\?list=[A-Za-z0-9_-]{2,200})$`)

type nativeRequest struct {
	Version int
	Action  string
	URL     string
	Mode    string
}

type nativeResponse struct {
	Version      int    `json:"version"`
	OK           bool   `json:"ok"`
	HostVersion  string `json:"hostVersion,omitempty"`
	State        string `json:"state,omitempty"`
	MPVAvailable *bool  `json:"mpvAvailable,omitempty"`
	Error        string `json:"error,omitempty"`
}

type nativeDependencies struct {
	inspect func() (connectionStatus, error)
	launch  func(context.Context, string, nativeRequest) error
}

func IsNativeInvocation(args []string) bool {
	if len(args) < 2 || args[1] == "-browser" {
		return false
	}
	if filepath.IsAbs(args[1]) && strings.EqualFold(filepath.Ext(args[1]), ".json") {
		return true
	}
	if strings.EqualFold(filepath.Base(args[1]), nativeHostName+".json") {
		return true
	}
	for _, arg := range args[2:] {
		if arg == extensionID {
			return true
		}
	}
	return false
}

func RunNative(ctx context.Context, version string, args []string, input io.Reader, output io.Writer) error {
	manifest, err := nativeManifestPath()
	if err != nil {
		return err
	}
	if err := validateNativeCaller(args, manifest); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, nativeStartupTimeout)
	defer cancel()
	response := serveNative(ctx, version, input, nativeDependencies{
		inspect: validateNativeOwner,
		launch: func(ctx context.Context, mpvPath string, request nativeRequest) error {
			return launchNativePlayback(ctx, executable, mpvPath, request)
		},
	})
	return writeNativeResponse(output, response)
}

func validateNativeCaller(args []string, manifest string) error {
	if len(args) != 3 || args[2] != extensionID || !filepath.IsAbs(args[1]) ||
		!strings.EqualFold(filepath.Clean(args[1]), filepath.Clean(manifest)) {
		return errors.New("Use YouTube Play in Firefox.")
	}
	return nil
}

func serveNative(ctx context.Context, version string, input io.Reader, deps nativeDependencies) nativeResponse {
	response := nativeResponse{Version: 1}
	request, err := readNativeRequestContext(ctx, input)
	if err != nil {
		response.Error = boundNativeError(err)
		return response
	}
	status, inspectErr := deps.inspect()
	if request.Action == "status" {
		response.HostVersion = version
		available := checkNativeMPV(status.MPVPath) == nil
		response.MPVAvailable = &available
		switch {
		case inspectErr != nil:
			response.State = "needs-attention"
			response.Error = boundNativeError(inspectErr)
		case status.State == "disabled":
			response.State = "disabled"
			response.Error = "Firefox integration disabled."
		case status.State != "enabled":
			response.State = "needs-attention"
			response.Error = boundNativeError(errors.New(status.Detail))
		case !available:
			response.State = "mpv-missing"
			response.Error = "mpv not found."
		default:
			response.State = "ready"
			response.OK = true
		}
		if !response.OK && response.Error == "" {
			response.Error = "Firefox integration needs attention."
		}
		return response
	}
	if inspectErr != nil {
		err = inspectErr
	} else if status.State != "enabled" {
		err = errors.New(status.Detail)
		if status.Detail == "" {
			err = errors.New("Firefox integration disabled. Run play -browser enable.")
		}
	} else if err = checkNativeMPV(status.MPVPath); err == nil {
		err = deps.launch(ctx, status.MPVPath, request)
	}
	if err != nil {
		response.Error = boundNativeError(err)
	} else {
		response.OK = true
	}
	return response
}

func readNativeRequestContext(ctx context.Context, input io.Reader) (nativeRequest, error) {
	if err := ctx.Err(); err != nil {
		return nativeRequest{}, err
	}
	if closer, ok := input.(io.Closer); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	request, err := readNativeRequest(input)
	if ctx.Err() != nil {
		return nativeRequest{}, ctx.Err()
	}
	return request, err
}

func readNativeRequest(input io.Reader) (nativeRequest, error) {
	var request nativeRequest
	var size uint32
	if err := binary.Read(input, binary.LittleEndian, &size); err != nil {
		return request, errors.New("Cannot read request header.")
	}
	if size == 0 || size > nativeMessageLimit {
		return request, errors.New("Invalid request size.")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(input, payload); err != nil {
		return request, errors.New("Incomplete request.")
	}
	if !utf8.Valid(payload) {
		return request, errors.New("Invalid request format.")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return request, errors.New("Invalid request format.")
	}
	fields := make(map[string]bool, 4)
	for decoder.More() {
		name, err := decoder.Token()
		key, ok := name.(string)
		if err != nil || !ok || fields[key] {
			return request, errors.New("Invalid request format.")
		}
		fields[key] = true
		switch key {
		case "version":
			err = decoder.Decode(&request.Version)
		case "action":
			err = decoder.Decode(&request.Action)
		case "url":
			err = decoder.Decode(&request.URL)
		case "mode":
			err = decoder.Decode(&request.Mode)
		default:
			return request, errors.New("Invalid request format.")
		}
		if err != nil {
			return request, errors.New("Invalid request format.")
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return request, errors.New("Invalid request format.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return request, errors.New("Invalid request format.")
	}
	if request.Version != 1 {
		return request, errors.New("Unsupported request version. Update Play and the extension.")
	}
	if fields["action"] {
		if len(fields) != 2 || request.Action != "status" {
			return request, errors.New("Unsupported request.")
		}
		return request, nil
	}
	if len(fields) != 3 || (request.Mode != "video" && request.Mode != "audio") || !nativePlaybackURL.MatchString(request.URL) {
		return request, errors.New("Unsupported request.")
	}
	if index := strings.Index(request.URL, "list="); index >= 0 && strings.HasPrefix(request.URL[index+5:], "RD") {
		return request, errors.New("Mix unsupported. Select a video.")
	}
	return request, nil
}

func checkNativeMPV(path string) error {
	if err := validateApplicationPath(path, true); err != nil {
		return errors.New("mpv not found. Run play -browser to select mpv.exe.")
	}
	return nil
}

func boundNativeError(err error) string {
	text := strings.ToValidUTF8(err.Error(), "")
	if len(text) > nativeDiagnosticLimit {
		text = text[:nativeDiagnosticLimit]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return text
}

func writeNativeResponse(output io.Writer, response nativeResponse) error {
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if err := binary.Write(output, binary.LittleEndian, uint32(len(payload))); err != nil {
		return err
	}
	_, err = io.Copy(output, bytes.NewReader(payload))
	return err
}
