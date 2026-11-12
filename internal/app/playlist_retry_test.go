package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"

	"play/internal/httpretry"
	"play/internal/media"
	localplaylist "play/internal/playlist"
	"play/internal/youtube"
)

func TestPlaylistPageHelperPreservesUpstreamCooldown(t *testing.T) {
	var calls atomic.Int32
	server, err := localplaylist.Start([]media.PlaylistItem{{VideoID: appTestID}}, func(context.Context, int, string) (localplaylist.ResolvedItem, error) {
		return localplaylist.ResolvedItem{}, errors.New("unused resolver")
	}, func(context.Context) ([]media.PlaylistItem, bool, error) {
		calls.Add(1)
		return nil, false, fmt.Errorf("load playlist: %w", httpretry.ErrDelayTooLong)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	directory := playlistPageDirectory(t)
	err = Run(context.Background(), "test", []string{"-playlist-page", server.PageURL() + "0002.m3u", directory}, nil, io.Discard, io.Discard)
	files, readErr := os.ReadDir(directory)
	if !errors.Is(err, httpretry.ErrDelayTooLong) || calls.Load() != 1 || readErr != nil || len(files) != 0 {
		t.Fatalf("error = %v, calls = %d, files = %v, read error = %v; want the upstream cooldown, one call and no files", err, calls.Load(), files, readErr)
	}
}

func TestPlaylistPageHelperKeepsOrdinaryFailureRecoverable(t *testing.T) {
	var calls atomic.Int32
	server, err := localplaylist.Start([]media.PlaylistItem{{VideoID: appTestID}}, func(context.Context, int, string) (localplaylist.ResolvedItem, error) {
		return localplaylist.ResolvedItem{}, errors.New("unused resolver")
	}, func(context.Context) ([]media.PlaylistItem, bool, error) {
		if calls.Add(1) == 1 {
			return nil, false, &youtube.TransportError{Kind: youtube.HTTPFailure, Status: http.StatusServiceUnavailable, RetryAfter: "0"}
		}
		return []media.PlaylistItem{{VideoID: "bbbbbbbbbbb", Title: "Recovered"}}, false, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	directory := playlistPageDirectory(t)
	args := []string{"-playlist-page", server.PageURL() + "0002.m3u", directory}
	if err := Run(context.Background(), "test", args, nil, io.Discard, io.Discard); err == nil || errors.Is(err, httpretry.ErrDelayTooLong) {
		t.Fatalf("error = %v; want a recoverable failure without a cooldown", err)
	}
	if err := Run(context.Background(), "test", args, nil, io.Discard, io.Discard); err != nil || calls.Load() != 2 {
		t.Fatalf("ordinary failure did not recover: calls = %d, error = %v", calls.Load(), err)
	}
}
