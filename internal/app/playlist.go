package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"play/internal/media"
	"play/internal/mpv"
	localplaylist "play/internal/playlist"
	"play/internal/youtube"
)

func runPlaylist(ctx context.Context, reference youtube.Reference, audioOnly bool, mpvPath string, errOut io.Writer, deps dependencies) error {
	diagnostics := &playlistDiagnostics{out: errOut}
	source, err := deps.openPlaylist(reference.PlaylistID)
	if err != nil {
		return fmt.Errorf("load playlist: %w", err)
	}

	var selectedItem media.Item
	var items []media.PlaylistItem
	var startIndex int
	var more bool
	if reference.VideoID != "" {
		selectedItem, items, startIndex, more, err = resolveReferencedPlaylist(ctx, reference, source, deps)
	} else {
		items, startIndex, more, err = resolvePlaylistStart(ctx, reference, source)
		if err == nil {
			selectedItem, err = resolveVideo(ctx, items[startIndex].VideoID, deps)
		}
	}
	if err != nil {
		return err
	}
	items[startIndex].Title = selectedItem.Title

	selectedPlan, err := makePlaybackPlan(selectedItem, audioOnly)
	if err != nil {
		return err
	}
	local, err := deps.startDynamicGateway(diagnostics.stream)
	if err != nil {
		return fmt.Errorf("start local stream server: %w", err)
	}
	if local.add == nil {
		return errors.Join(errors.New("local stream server does not support playlist resources"), gatewayCloseError(local))
	}

	selectedEntry, err := resolvePlaylistItem(selectedItem, selectedPlan, startIndex, local)
	if err != nil {
		return errors.Join(err, gatewayCloseError(local))
	}

	resolver := func(ctx context.Context, index int, videoID string) (localplaylist.ResolvedItem, error) {
		if index == startIndex {
			return selectedEntry, nil
		}
		item, err := resolveVideo(ctx, videoID, deps)
		if err != nil {
			return localplaylist.ResolvedItem{}, err
		}
		plan, err := makePlaybackPlan(item, audioOnly)
		if err != nil {
			return localplaylist.ResolvedItem{}, err
		}
		return resolvePlaylistItem(item, plan, index, local)
	}

	var pageLoader localplaylist.PageLoader
	if more {
		pageLoader = func(ctx context.Context) ([]media.PlaylistItem, bool, error) {
			page, more, err := source.Next(ctx)
			if err != nil {
				err = fmt.Errorf("load playlist: %w", err)
				if ctx.Err() == nil {
					diagnostics.page(err)
				}
				return nil, false, err
			}
			return page, more, nil
		}
	}

	playlistServer, err := localplaylist.Start(items, resolver, pageLoader, diagnostics.item)
	if err != nil {
		return errors.Join(fmt.Errorf("start local playlist server: %w", err), gatewayCloseError(local))
	}
	if err := playlistServer.Prepare(ctx, startIndex); err != nil {
		return errors.Join(err, playlistServerCloseError(playlistServer), gatewayCloseError(local))
	}

	playErr := deps.playPlaylist(ctx, mpv.PlaylistRequest{
		Executable:   mpvPath,
		URL:          playlistServer.URL(),
		PageURL:      playlistServer.PageURL(),
		StartIndex:   startIndex,
		StartSeconds: reference.StartSeconds,
		AudioOnly:    audioOnly,
	})
	return errors.Join(playErr, playlistServerCloseError(playlistServer), gatewayCloseError(local))
}

type videoResult struct {
	item media.Item
	err  error
}

type playlistResult struct {
	items      []media.PlaylistItem
	startIndex int
	more       bool
	err        error
}

func resolveReferencedPlaylist(ctx context.Context, reference youtube.Reference, source playlistSource, deps dependencies) (media.Item, []media.PlaylistItem, int, bool, error) {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	videoCh := make(chan videoResult, 1)
	playlistCh := make(chan playlistResult, 1)
	go func() {
		item, err := resolveVideo(workCtx, reference.VideoID, deps)
		videoCh <- videoResult{item: item, err: err}
	}()
	go func() {
		items, startIndex, more, err := resolvePlaylistStart(workCtx, reference, source)
		playlistCh <- playlistResult{items: items, startIndex: startIndex, more: more, err: err}
	}()

	var video media.Item
	var playlist playlistResult
	for videoCh != nil || playlistCh != nil {
		select {
		case <-ctx.Done():
			return media.Item{}, nil, 0, false, ctx.Err()
		case result := <-videoCh:
			videoCh = nil
			if result.err != nil {
				cancel()
				return media.Item{}, nil, 0, false, result.err
			}
			video = result.item
		case result := <-playlistCh:
			playlistCh = nil
			if result.err != nil {
				cancel()
				return media.Item{}, nil, 0, false, result.err
			}
			playlist = result
		}
	}
	return video, playlist.items, playlist.startIndex, playlist.more, nil
}

func resolvePlaylistStart(ctx context.Context, reference youtube.Reference, source playlistSource) ([]media.PlaylistItem, int, bool, error) {
	var items []media.PlaylistItem
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, err
		}
		page, more, err := source.Next(ctx)
		if err != nil {
			return nil, 0, false, fmt.Errorf("load playlist: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, 0, false, err
		}
		if len(page) == 0 && more {
			return nil, 0, false, errors.New("playlist page loader returned an empty page before completion")
		}
		items = append(items, page...)
		if index, ready, err := playlistStartIndex(items, reference, !more); ready || err != nil {
			return items, index, more, err
		}
	}
}

func resolvePlaylistItem(item media.Item, plan playbackPlan, index int, local gateway) (localplaylist.ResolvedItem, error) {
	namespacePlaybackPlan(&plan, index)
	request, err := makePlaybackMetadata(item, 0, plan)
	if err != nil {
		return localplaylist.ResolvedItem{}, err
	}
	if err := local.add(plan.resources); err != nil {
		return localplaylist.ResolvedItem{}, fmt.Errorf("add playlist streams: %w", err)
	}
	if err := resolvePlaybackURLs(&request, plan, local); err != nil {
		return localplaylist.ResolvedItem{}, err
	}
	body, err := mpv.EDL(request)
	if err != nil {
		return localplaylist.ResolvedItem{}, err
	}
	return localplaylist.ResolvedItem{EDL: body, CoverURL: request.CoverURL, PreviewURL: request.PreviewURL}, nil
}

func playlistStartIndex(items []media.PlaylistItem, reference youtube.Reference, complete bool) (int, bool, error) {
	if reference.PlaylistIndex > 0 {
		index := reference.PlaylistIndex - 1
		if index >= len(items) {
			if !complete {
				return 0, false, nil
			}
			if reference.VideoID == "" {
				return 0, true, errors.New("the requested playlist position does not exist")
			}
		} else {
			if reference.VideoID == "" || items[index].VideoID == reference.VideoID {
				return index, true, nil
			}
			if found := playlistVideoIndex(items, reference.VideoID); found >= 0 {
				return found, true, nil
			}
			if !complete {
				return 0, false, nil
			}
		}
	}
	if reference.VideoID != "" {
		if index := playlistVideoIndex(items, reference.VideoID); index >= 0 {
			return index, true, nil
		}
		if !complete {
			return 0, false, nil
		}
		return 0, true, errors.New("the requested video is not in this playlist")
	}
	if len(items) > 0 {
		return 0, true, nil
	}
	if complete {
		return 0, true, errors.New("playlist contains no videos")
	}
	return 0, false, nil
}

func playlistVideoIndex(items []media.PlaylistItem, videoID string) int {
	for i, item := range items {
		if item.VideoID == videoID {
			return i
		}
	}
	return -1
}

func namespacePlaybackPlan(plan *playbackPlan, index int) {
	prefix := fmt.Sprintf("p%04d-", index+1)
	for i := range plan.resources {
		plan.resources[i].Name = prefix + plan.resources[i].Name
	}
	for i := range plan.tracks {
		plan.tracks[i].name = prefix + plan.tracks[i].name
	}
	if plan.cover != "" {
		plan.cover = prefix + plan.cover
	}
	if plan.preview != "" {
		plan.preview = prefix + plan.preview
	}
}

func playlistServerCloseError(server *localplaylist.Server) error {
	if err := server.Close(); err != nil {
		return fmt.Errorf("stop local playlist server: %w", err)
	}
	return nil
}

type playlistDiagnostics struct {
	mu  sync.Mutex
	out io.Writer
}

func (d *playlistDiagnostics) write(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.out, format, args...)
}

func (d *playlistDiagnostics) stream(name string, err error) {
	d.write("play: stream %s: %v\n", name, err)
}

func (d *playlistDiagnostics) item(index int, err error) {
	d.write("play: playlist item %d: %v\n", index+1, err)
}

func (d *playlistDiagnostics) page(err error) {
	d.write("play: playlist: %v\n", err)
}
