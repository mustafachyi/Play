package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"play/internal/httpretry"
)

const maxPlaylistPageSize = 16 << 20

func fetchPlaylistPage(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("playlist page request is invalid")
	}
	target, ok := playlistPageURL(args[0])
	if !ok || !ownedPlaylistDirectory(args[1]) {
		return errors.New("playlist page request is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return errors.New("playlist page request is invalid")
	}
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch playlist page: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusServiceUnavailable {
		return httpretry.ErrDelayTooLong
	}
	if response.StatusCode != http.StatusOK {
		return errors.New("playlist page is unavailable")
	}
	if response.ContentLength > maxPlaylistPageSize {
		return errors.New("playlist page is too large")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPlaylistPageSize+1))
	if err != nil {
		return fmt.Errorf("read playlist page: %w", err)
	}
	if len(body) > maxPlaylistPageSize || !validPlaylistPage(body, target) {
		return errors.New("playlist page is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.CreateTemp(args[1], ".page-*.m3u")
	if err != nil {
		return fmt.Errorf("create playlist page: %w", err)
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	_, writeErr := file.Write(body)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write playlist page: %w", err)
	}
	destination := filepath.Join(args[1], filepath.Base(target.Path))
	if err := os.Rename(name, destination); err != nil {
		return fmt.Errorf("publish playlist page: %w", err)
	}
	return nil
}

func playlistPageURL(value string) (*url.URL, bool) {
	if len(value) > 4096 {
		return nil, false
	}
	target, err := url.Parse(value)
	if err != nil || target.Scheme != "http" || target.Hostname() != "127.0.0.1" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || target.RawPath != "" {
		return nil, false
	}
	port, err := strconv.Atoi(target.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, false
	}
	parts := strings.Split(strings.TrimPrefix(target.Path, "/"), "/")
	if len(parts) != 3 || len(parts[0]) != 32 || parts[1] != "page" || len(parts[2]) != 8 || !strings.HasSuffix(parts[2], ".m3u") {
		return nil, false
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return nil, false
	}
	page, err := strconv.Atoi(strings.TrimSuffix(parts[2], ".m3u"))
	if err != nil || page < 2 || fmt.Sprintf("%04d.m3u", page) != parts[2] {
		return nil, false
	}
	return target, true
}

func ownedPlaylistDirectory(value string) bool {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || filepath.Dir(value) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(value), "play-pages-") {
		return false
	}
	info, err := os.Lstat(value)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func validPlaylistPage(body []byte, target *url.URL) bool {
	if !bytes.HasPrefix(body, []byte("#EXTM3U\n")) || !bytes.HasSuffix(body, []byte("\n")) {
		return false
	}
	prefix := []byte(target.Scheme + "://" + target.Host + strings.Split(target.Path, "/page/")[0] + "/item/")
	remaining := body[len("#EXTM3U\n"):]
	for len(remaining) > 0 {
		title, tail, ok := bytes.Cut(remaining, []byte{'\n'})
		if !ok || !bytes.HasPrefix(title, []byte("#EXTINF:-1,")) || bytes.ContainsAny(title, "\r\x00") {
			return false
		}
		entry, tail, ok := bytes.Cut(tail, []byte{'\n'})
		if !ok || !bytes.HasPrefix(entry, prefix) {
			return false
		}
		name := entry[len(prefix):]
		if len(name) != 8 || !bytes.HasSuffix(name, []byte(".edl")) || bytes.Equal(name[:4], []byte("0000")) {
			return false
		}
		for _, digit := range name[:4] {
			if digit < '0' || digit > '9' {
				return false
			}
		}
		remaining = tail
	}
	return true
}
