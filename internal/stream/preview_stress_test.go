package stream

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"io"
	"math"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"play/internal/media"
)

func TestPreviewValidationRejectsUnsafeSources(t *testing.T) {
	for _, source := range []*PreviewSource{nil, {}, {ArtworkURLs: []string{"http://example.com/image"}}, {ArtworkURLs: []string{"https://user:password@example.com/image"}}} {
		if err := validatePreview(source); err == nil {
			t.Fatalf("invalid source was accepted: %#v", source)
		}
	}
	for _, mutate := range []func(*media.Storyboard){
		func(board *media.Storyboard) { board.Width = 0 },
		func(board *media.Storyboard) { board.Width = 1025 },
		func(board *media.Storyboard) { board.Height = 0 },
		func(board *media.Storyboard) { board.Height = 1025 },
		func(board *media.Storyboard) { board.Columns = 0 },
		func(board *media.Storyboard) { board.Columns = math.MaxInt },
		func(board *media.Storyboard) { board.Rows = 0 },
		func(board *media.Storyboard) { board.Rows = math.MaxInt },
		func(board *media.Storyboard) { board.Columns, board.Rows = 11, 10 },
		func(board *media.Storyboard) { board.Count = 0 },
		func(board *media.Storyboard) { board.Count = 1_000_001 },
		func(board *media.Storyboard) { board.Interval = 0 },
		func(board *media.Storyboard) { board.Interval = math.NaN() },
		func(board *media.Storyboard) { board.Interval = math.Inf(1) },
		func(board *media.Storyboard) { board.Count, board.URL = 26, "https://example.com/static.jpg" },
	} {
		board := &media.Storyboard{URL: "https://example.com/$M", Width: 160, Height: 90, Columns: 5, Rows: 5, Count: 25, Interval: 2}
		mutate(board)
		if err := validatePreview(&PreviewSource{Storyboard: board}); err == nil {
			t.Fatalf("unsafe storyboard was accepted: %#v", board)
		}
	}
	board := &media.Storyboard{URL: "https://example.com/$M", Width: 160, Height: 90, Columns: 5, Rows: 5, Count: 25, Interval: 2}
	if err := validatePreview(&PreviewSource{Storyboard: board, ArtworkURLs: []string{"https://example.com/image"}}); err == nil {
		t.Fatal("conflicting preview sources were accepted")
	}
}

func TestPreviewRejectsOversizedPNGBeforePixelDecode(t *testing.T) {
	for _, dimensions := range []image.Point{{maxPreviewPixels + 1, 1}, {4096, 2049}} {
		chunk := make([]byte, 25)
		binary.BigEndian.PutUint32(chunk[:4], 13)
		copy(chunk[4:8], "IHDR")
		binary.BigEndian.PutUint32(chunk[8:12], uint32(dimensions.X))
		binary.BigEndian.PutUint32(chunk[12:16], uint32(dimensions.Y))
		chunk[16], chunk[17] = 8, 6
		binary.BigEndian.PutUint32(chunk[21:25], crc32.ChecksumIEEE(chunk[4:21]))
		payload := append([]byte{137, 80, 78, 71, 13, 10, 26, 10}, chunk...)
		if config, _, err := image.DecodeConfig(bytes.NewReader(payload)); err != nil || config.Width != dimensions.X || config.Height != dimensions.Y {
			t.Fatalf("oversized fixture config = %#v, error = %v", config, err)
		}
		cache := newPreviewCache(previewCacheLimit)
		handler := &previewHandler{cache: cache, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
		})}}
		if _, err := handler.image(context.Background(), []string{"https://example.com/oversized.png"}); err == nil {
			t.Fatal("oversized preview image was accepted")
		}
		if cache.image != nil || cache.directory != "" || cache.bytes != 0 {
			t.Fatal("oversized image populated the cache")
		}
		if err := cache.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPreviewCacheCanceledHitDoesNotReturnMetadata(t *testing.T) {
	cache := newPreviewCache(8)
	defer cache.Close()
	metadata, err := cache.get(context.Background(), "page", func() ([]byte, previewMetadata, error) {
		return []byte{1, 2, 3, 4}, previewMetadata{}, nil
	})
	if err != nil || metadata.Path == "" {
		t.Fatalf("initial preview = %#v, error = %v", metadata, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	metadata, err = cache.get(ctx, "page", func() ([]byte, previewMetadata, error) {
		t.Fatal("a canceled hit entered the loader")
		return nil, previewMetadata{}, nil
	})
	if !errors.Is(err, context.Canceled) || metadata.Path != "" {
		t.Fatalf("canceled cached preview = %#v, error = %v", metadata, err)
	}
}

func TestPreviewCacheCloseReleasesQueuedRequestsBeforeLoaderFinishes(t *testing.T) {
	cache := newPreviewCache(8)
	started, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() {
		released.Do(func() { close(release) })
		workers.Wait()
		if err := cache.Close(); err != nil {
			t.Error(err)
		}
	})
	active := make(chan error, 1)
	workers.Go(func() {
		_, err := cache.get(context.Background(), "active", func() ([]byte, previewMetadata, error) {
			close(started)
			<-release
			return []byte{1, 2, 3, 4}, previewMetadata{}, nil
		})
		active <- err
	})
	<-started
	const waiters = 64
	queued := make(chan error, waiters)
	for index := range waiters {
		workers.Go(func() {
			_, err := cache.get(context.Background(), fmt.Sprintf("queued-%d", index), func() ([]byte, previewMetadata, error) {
				t.Error("a queued loader started after closure")
				return []byte{1, 2, 3, 4}, previewMetadata{}, nil
			})
			queued <- err
		})
	}
	select {
	case err := <-queued:
		t.Fatalf("queued request returned before closure: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for range waiters {
		select {
		case err := <-queued:
			if err == nil {
				t.Fatal("closed cache accepted a queued request")
			}
		case <-deadline.C:
			t.Fatal("closed cache retained queued requests behind its active loader")
		}
	}
	released.Do(func() { close(release) })
	if err := <-active; err == nil {
		t.Fatal("a completed loader published after closure")
	}
	if cache.bytes != 0 || cache.directory != "" || len(cache.entries) != 0 {
		t.Fatal("closed cache retained a late preview file")
	}
}

func TestPreviewCacheConcurrentEvictionMaintainsStorageAccounting(t *testing.T) {
	cache := newPreviewCache(32)
	defer cache.Close()
	var workers sync.WaitGroup
	for index := range 128 {
		workers.Go(func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if index%5 == 0 {
				cancel()
			}
			_, err := cache.get(ctx, fmt.Sprintf("page-%d", index%17), func() ([]byte, previewMetadata, error) {
				return []byte{byte(index), 2, 3, 4}, previewMetadata{}, nil
			})
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	var retained int64
	for key, element := range cache.entries {
		entry := element.Value.(previewPage)
		info, err := os.Stat(entry.metadata.Path)
		if err != nil || info.Size() != entry.size || entry.key != key {
			t.Fatalf("retained page %q accounting failed: %#v, %v", key, entry, err)
		}
		retained += entry.size
	}
	if retained != cache.bytes || retained > cache.limit || cache.order.Len() != len(cache.entries) {
		t.Fatalf("retained bytes = %d, accounted = %d, limit = %d, LRU = %d, entries = %d", retained, cache.bytes, cache.limit, cache.order.Len(), len(cache.entries))
	}
}

func TestPreviewCacheBoundsTinyFilesAndRecoversMissingEntries(t *testing.T) {
	cache := newPreviewCache(previewCacheLimit)
	defer cache.Close()
	var loads int
	load := func() ([]byte, previewMetadata, error) {
		loads++
		return []byte{1, 2, 3, 4}, previewMetadata{}, nil
	}
	first, err := cache.get(context.Background(), "first", load)
	if err != nil {
		t.Fatal(err)
	}
	for index := range previewEntryLimit {
		if _, err := cache.get(context.Background(), fmt.Sprintf("tiny-%d", index), load); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) != previewEntryLimit || cache.bytes != previewEntryLimit*4 || cache.order.Len() != previewEntryLimit {
		t.Fatalf("tiny cache entries = %d, bytes = %d, LRU = %d", len(cache.entries), cache.bytes, cache.order.Len())
	}
	if _, err := os.Stat(first.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("entry-limit eviction left the oldest file: %v", err)
	}
	before := loads
	hit, err := cache.get(context.Background(), "tiny-0", load)
	if err != nil || loads != before {
		t.Fatalf("retained entry reload count = %d; before = %d, error = %v", loads, before, err)
	}
	if err := os.Remove(hit.Path); err != nil {
		t.Fatal(err)
	}
	replaced, err := cache.get(context.Background(), "tiny-0", load)
	if err != nil || replaced.Path == hit.Path || loads != before+1 || len(cache.entries) != previewEntryLimit || cache.bytes != previewEntryLimit*4 {
		t.Fatalf("missing-entry replacement = %#v, loads = %d, entries = %d, bytes = %d, error = %v", replaced, loads, len(cache.entries), cache.bytes, err)
	}
	if _, err := cache.get(context.Background(), "extra", load); err != nil {
		t.Fatal(err)
	}
	if _, found, err := cache.lookup("tiny-0"); !found || err != nil {
		t.Fatalf("recent replacement was evicted instead of the oldest entry: found = %v, error = %v", found, err)
	}
	files, err := os.ReadDir(cache.directory)
	if err != nil || len(files) != previewEntryLimit {
		t.Fatalf("cache file count = %d, error = %v", len(files), err)
	}
}

func TestPreviewCacheConcurrentDecodedReplacementAndClose(t *testing.T) {
	cache := newPreviewCache(8)
	var workers sync.WaitGroup
	var unexpected atomic.Int32
	for index := range 128 {
		workers.Go(func() {
			key := fmt.Sprintf("image-%d", index)
			pixels := image.NewRGBA(image.Rect(0, 0, 2, 2))
			if err := cache.retainDecoded(context.Background(), key, pixels); err != nil {
				return
			}
			got, err := cache.decoded(key)
			if err == nil && got != nil && got != pixels {
				unexpected.Add(1)
			}
		})
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if unexpected.Load() != 0 || cache.image != nil || cache.imageKey != "" {
		t.Fatal("decoded image replacement or closure retained inconsistent state")
	}
}

func FuzzPreviewRequestBounds(f *testing.F) {
	for _, raw := range []string{"", "page=0", "page=1&width=80&height=60", "time=NaN", "time=1e308", "time=0&width=3&height=3", "page=10000000000000000000", "page=1&page=2"} {
		f.Add(raw)
	}
	handler := &previewHandler{source: Resource{Preview: &PreviewSource{Storyboard: &media.Storyboard{Width: 160, Height: 90, Columns: 5, Rows: 5, Count: 103, Interval: math.SmallestNonzeroFloat64}}}}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 4096 {
			t.Skip()
		}
		request, err := handler.request(raw)
		if err != nil {
			return
		}
		if request.page < 0 || request.page > 4 || (request.maxWidth == 0) != (request.maxHeight == 0) || request.maxWidth != 0 && (request.maxWidth < 3 || request.maxWidth > 512 || request.maxHeight < 3 || request.maxHeight > 512) {
			t.Fatalf("accepted out-of-bounds query %q: %#v", raw, request)
		}
	})
}
