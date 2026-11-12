package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func fetchPreview(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 1 || !validPreviewURL(args[0]) {
		return errors.New("preview request is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, args[0], nil)
	if err != nil {
		return errors.New("preview request is invalid")
	}
	client := &http.Client{
		Timeout:       6 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch preview: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("preview is unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil {
		return fmt.Errorf("read preview metadata: %w", err)
	}
	if len(body) > 8192 || !json.Valid(body) {
		return errors.New("preview metadata is invalid")
	}
	if _, err := out.Write(body); err != nil {
		return fmt.Errorf("write preview metadata: %w", err)
	}
	return nil
}

func validPreviewURL(value string) bool {
	if len(value) > 4096 {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 2 || len(parts[0]) != 32 {
		return false
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return false
	}
	return parts[1] == "preview.json" || strings.HasSuffix(parts[1], "-preview.json")
}
