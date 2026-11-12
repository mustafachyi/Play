package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"play/internal/media"
)

func previewImage(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			value := color.RGBA{R: 255, A: 255}
			if x >= 20 {
				value = color.RGBA{B: 255, A: 255}
			}
			canvas.SetRGBA(x, y, value)
		}
	}
	var body bytes.Buffer
	if err := png.Encode(&body, canvas); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestPreviewPagesLoadLazilyAndCachePackedFrames(t *testing.T) {
	payload := previewImage(t)
	var requests atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()
	board := &media.Storyboard{URL: upstream.URL + "/$M", Width: 20, Height: 20, Columns: 2, Rows: 1, Count: 3, Interval: 2}
	server, err := Start([]Resource{{Name: "preview.json", Preview: &PreviewSource{Storyboard: board}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.client.Transport = upstream.Client().Transport
	if requests.Load() != 0 || server.previews.directory != "" {
		t.Fatal("preview was fetched or cached before the first request")
	}
	request := func(query string) previewMetadata {
		t.Helper()
		resp, err := http.Get(server.URL("preview.json") + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("preview status = %d", resp.StatusCode)
		}
		var metadata previewMetadata
		if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
			t.Fatal(err)
		}
		return metadata
	}
	first := request("?time=1")
	if first.Width != 143 || first.Height != 143 || first.Count != 3 || first.FramesPerPage != 2 || first.Interval != 2 || first.Page != 0 {
		t.Fatalf("metadata = %#v", first)
	}
	body, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	frameBytes := first.Width * first.Height * 4
	if len(body) != frameBytes*2 {
		t.Fatalf("frame bytes = %d", len(body))
	}
	center := ((first.Height/2)*first.Width + first.Width/2) * 4
	if !bytes.Equal(body[center:center+4], []byte{0, 0, 255, 255}) || !bytes.Equal(body[frameBytes+center:frameBytes+center+4], []byte{255, 0, 0, 255}) {
		t.Fatal("frame order or BGRA channels are incorrect")
	}
	if !bytes.Equal(body[:4], []byte{0, 0, 0, 0}) || !bytes.Equal(body[(first.Width/2)*4:(first.Width/2)*4+4], []byte{255, 255, 255, 255}) {
		t.Fatal("rounded corner or white border is incorrect")
	}
	for offset := 0; offset < len(body); offset += 4 {
		if body[offset] > body[offset+3] || body[offset+1] > body[offset+3] || body[offset+2] > body[offset+3] {
			t.Fatal("BGRA color channels are not premultiplied by alpha")
		}
	}
	if second := request("?time=3"); second.Path != first.Path || requests.Load() != 1 {
		t.Fatal("cached preview page was not reused")
	}
	last := request("?time=999")
	lastBody, err := os.ReadFile(last.Path)
	if err != nil {
		t.Fatal(err)
	}
	if last.Page != 1 || len(lastBody) != frameBytes || requests.Load() != 2 {
		t.Fatalf("last page = %#v, bytes = %d, requests = %d", last, len(lastBody), requests.Load())
	}
	directory := server.previews.directory
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache directory stat error = %v; want os.ErrNotExist", err)
	}
}

func TestArtworkPreviewUsesCompatibleFallbackOnce(t *testing.T) {
	payload := previewImage(t)
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		body := payload
		if r.URL.Path == "/unsupported" {
			body = []byte("unsupported codec")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	cache := newPreviewCache(previewCacheLimit)
	defer cache.Close()
	handler := &previewHandler{client: client, cache: cache, source: Resource{Name: "artwork", Preview: &PreviewSource{ArtworkURLs: []string{"https://example.com/unsupported", "https://example.com/artwork"}}}}
	var first previewMetadata
	for _, query := range []string{"?time=0", "?time=1000", "?page=0"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://local/artwork"+query, nil))
		if response.Code != 200 {
			t.Fatalf("artwork status = %d; want 200", response.Code)
		}
		var metadata previewMetadata
		if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
			t.Fatal(err)
		}
		if first.Path == "" {
			first = metadata
		}
		if metadata.Path != first.Path || metadata.Count != 1 || metadata.Interval != 0 || metadata.Width != 245 || metadata.Height != 124 {
			t.Fatalf("metadata = %#v", metadata)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d", requests.Load())
	}
}

func TestPreviewRejectsInvalidRequestsWithoutFetching(t *testing.T) {
	cache := newPreviewCache(previewCacheLimit)
	defer cache.Close()
	var requests atomic.Int32
	handler := &previewHandler{cache: cache, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected fetch")
	})}, source: Resource{Name: "preview", Preview: &PreviewSource{Storyboard: &media.Storyboard{URL: "https://example.com/$M", Width: 20, Height: 20, Columns: 2, Rows: 1, Count: 3, Interval: 2}}}}
	for _, query := range []string{"?page=-1", "?page=2", "?page=1&time=2", "?time=NaN", "?time=Inf", "?time=-1", "?time=", "?time=2&time=3", "?other=1", "?bad=%xx", "?width=80", "?height=60", "?width=2&height=60", "?width=80&height=513", "?width=-3&height=60", "?width=80.5&height=60", "?width=NaN&height=60", "?width=80&height=60&width=90", "?time=1&page=0&width=80&height=60", "?width=80&height=60&other=1", "?width=65536&height=60"} {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "http://local/preview"+query, nil))
		if r.Code != 400 {
			t.Fatalf("query = %s, status = %d", query, r.Code)
		}
	}
	for _, method := range []string{http.MethodHead, http.MethodPost} {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(method, "http://local/preview", nil))
		if method == http.MethodHead && r.Code != 200 || method == http.MethodPost && r.Code != 405 {
			t.Fatalf("method = %s, status = %d", method, r.Code)
		}
	}
	if requests.Load() != 0 || cache.directory != "" {
		t.Fatal("invalid or HEAD request fetched or cached a preview")
	}
}

func TestPreviewLayoutMatchesMeasuredStoryboardCells(t *testing.T) {
	for _, test := range []struct {
		name          string
		width, height int
		displayWidth  float64
		displayHeight float64
		canvasWidth   int
		canvasHeight  int
	}{
		{"loaded high resolution", 320, 180, 242.36666666666667, 138, 243, 138},
		{"loaded medium resolution", 160, 90, 243.68333333333334, 138, 244, 138},
		{"low resolution fallback", 80, 45, 244.33333333333334, 137.66666666666666, 245, 138},
		{"portrait", 90, 160, 78.73333333333333, 143, 79, 143},
		{"square", 160, 160, 140.73333333333332, 143, 141, 143},
		{"four by three", 320, 240, 186.6, 143, 187, 143},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := previewLayout(test.width, test.height, 0, 0, true)
			if math.Abs(got.displayWidth-test.displayWidth) > 1e-9 || math.Abs(got.displayHeight-test.displayHeight) > 1e-9 || got.width != test.canvasWidth || got.height != test.canvasHeight {
				t.Fatalf("geometry = %#v; want logical %gx%g, physical %dx%d", got, test.displayWidth, test.displayHeight, test.canvasWidth, test.canvasHeight)
			}
		})
	}
}

func TestPreviewLayoutConstrainsContentWithoutScalingBorder(t *testing.T) {
	full := previewLayout(320, 180, 0, 0, true)
	for _, bounds := range []image.Point{{80, 60}, {300, 40}, {3, 3}, {512, 512}, {242, 138}} {
		got := previewLayout(320, 180, bounds.X, bounds.Y, true)
		if got.width > bounds.X || got.height > bounds.Y || got.width < 3 || got.height < 3 {
			t.Fatalf("bounds = %v, geometry = %#v", bounds, got)
		}
		if math.Abs(got.scaledWidth/full.scaledWidth-got.scaledHeight/full.scaledHeight) > 1e-12 {
			t.Fatalf("source scale differs between axes: %#v", got)
		}
		mask := previewMask(got.displayWidth, got.displayHeight)
		middleLeft := mask[(got.height/2)*got.width]
		if middleLeft.interior != 0 || middleLeft.border == 0 || got.width >= 16 && got.height >= 16 && middleLeft.border != 255 {
			t.Fatalf("bounds = %v, left border = %#v; want an outside frame with the rounded-corner coverage preserved", bounds, middleLeft)
		}
	}
	for _, size := range []image.Point{{1, 1024}, {1024, 1}, {1, 1}} {
		got := previewLayout(size.X, size.Y, 3, 3, true)
		if got.width != 3 || got.height != 3 || got.scaledWidth <= 0 || got.scaledHeight <= 0 {
			t.Fatalf("tiny source %v produced invalid geometry: %#v", size, got)
		}
	}
}

func TestArtworkPreviewPreservesFullImageAspectWithinFixedFrame(t *testing.T) {
	for _, source := range []image.Point{{3840, 2160}, {720, 1280}, {480, 480}, {40, 20}} {
		for _, bounds := range []image.Point{{0, 0}, {80, 60}, {300, 40}} {
			geometry := previewLayout(source.X, source.Y, bounds.X, bounds.Y, false)
			ratio := float64(source.X) / float64(source.Y)
			if math.Abs(geometry.scaledWidth/geometry.scaledHeight-ratio) > 1e-12 {
				t.Fatalf("artwork %v changed aspect: %#v", source, geometry)
			}
			if math.Abs((geometry.displayWidth-2)-geometry.scaledWidth) > 1.0/120+1e-12 || math.Abs((geometry.displayHeight-2)-geometry.scaledHeight) > 1.0/120+1e-12 {
				t.Fatalf("artwork %v was clipped instead of rounded to logical pixels: %#v", source, geometry)
			}
			if bounds.X > 0 && (geometry.width > bounds.X || geometry.height > bounds.Y) {
				t.Fatalf("artwork %v exceeds bounds %v: %#v", source, bounds, geometry)
			}
		}
	}
}

func TestPreviewFractionalEdgeRetainsLogicalWidthAndPremultipliedAlpha(t *testing.T) {
	geometry := previewLayout(320, 180, 0, 0, true)
	mask := previewMask(geometry.displayWidth, geometry.displayHeight)
	edge := mask[60*geometry.width+geometry.width-1]
	if edge.interior != 0 || edge.border != 94 {
		t.Fatalf("fractional right edge = %#v; want white alpha94", edge)
	}
	source := image.NewRGBA(image.Rect(0, 0, 320, 180))
	draw.Draw(source, source.Bounds(), &image.Uniform{C: color.RGBA{R: 255, A: 255}}, image.Point{}, draw.Src)
	output := make([]byte, geometry.width*geometry.height*4)
	xs := previewContentSamples(320, geometry.width, geometry.scaledWidth)
	ys := previewContentSamples(180, geometry.height, geometry.scaledHeight)
	if xs[240].first != 314 || xs[240].second != 315 {
		t.Fatalf("right visible sample = %#v; want the original scaled cell clipped on its right", xs[240])
	}
	if err := renderPreview(context.Background(), output, source, source.Bounds(), xs, ys, mask); err != nil {
		t.Fatal(err)
	}
	offset := (60*geometry.width + geometry.width - 1) * 4
	if !bytes.Equal(output[offset:offset+4], []byte{94, 94, 94, 94}) {
		t.Fatalf("fractional frame edge = %v; want premultiplied white alpha94", output[offset:offset+4])
	}
	inside := (60*geometry.width + 1) * 4
	if !bytes.Equal(output[inside:inside+4], []byte{0, 0, 255, 255}) {
		t.Fatalf("first content pixel = %v; frame consumed content", output[inside:inside+4])
	}
}

func TestPreviewResizeCachesVariantsAndReusesDecodedSheet(t *testing.T) {
	payload := previewImage(t)
	var requests atomic.Int32
	cache := newPreviewCache(previewCacheLimit)
	defer cache.Close()
	handler := &previewHandler{cache: cache, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload))}, nil
	})}, source: Resource{Name: "preview", Preview: &PreviewSource{Storyboard: &media.Storyboard{URL: "https://example.com/$M", Width: 20, Height: 20, Columns: 2, Rows: 1, Count: 3, Interval: 2}}}}
	fetch := func(query string) previewMetadata {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://local/preview"+query, nil))
		var data previewMetadata
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &data) != nil {
			t.Fatalf("query %s returned %d: %s", query, response.Code, response.Body.String())
		}
		return data
	}
	large := fetch("?time=0")
	small := fetch("?time=0&width=80&height=60")
	if small.Path == large.Path || small.Width > 80 || small.Height > 60 || small.MaxWidth != 80 || small.MaxHeight != 60 || requests.Load() != 1 {
		t.Fatalf("resize metadata = %#v, original = %#v, requests = %d", small, large, requests.Load())
	}
	if again := fetch("?page=0&width=80&height=60"); again != small {
		t.Fatalf("same size page was not reused: %#v; want %#v", again, small)
	}
	if again := fetch("?time=0"); again != large {
		t.Fatalf("full size cache changed after resizing: %#v", again)
	}
	_ = fetch("?time=6&width=80&height=60")
	if requests.Load() != 2 {
		t.Fatalf("new sheet requests = %d; want 2", requests.Load())
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if cache.image != nil || cache.imageKey != "" {
		t.Fatal("closed cache retained its decoded source")
	}
}

func TestPreviewUsesDeclaredAspectAndActualSpriteCellPitch(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 800, 450))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.RGBA{R: 255, A: 255}}, image.Point{}, draw.Src)
	for column := range 5 {
		draw.Draw(canvas, image.Rect(column*160+3, 0, column*160+6, 450), &image.Uniform{C: color.RGBA{B: 255, A: 255}}, image.Point{}, draw.Src)
	}
	var payload bytes.Buffer
	if err := png.Encode(&payload, canvas); err != nil {
		t.Fatal(err)
	}
	handler := &previewHandler{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload.Bytes()))}, nil
	})}, source: Resource{Preview: &PreviewSource{Storyboard: &media.Storyboard{URL: "https://example.com/$M", Width: 159, Height: 90, Columns: 5, Rows: 5, Count: 25, Interval: 2}}}}
	data, metadata, err := handler.render(context.Background(), previewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Width != 244 || metadata.Height != 139 || metadata.DisplayWidth != 243.68333333333334 || metadata.DisplayHeight != 139 {
		t.Fatalf("declared159x90 aspect was replaced by natural160x90: %#v", metadata)
	}
	frameBytes := metadata.Width * metadata.Height * 4
	for _, frame := range []int{0, 4, 9, 24} {
		offset := frame*frameBytes + (30*metadata.Width+6)*4
		if !bytes.Equal(data[offset:offset+4], []byte{255, 0, 0, 255}) {
			t.Fatalf("frame %d sprite marker = %v; natural160px column pitch was lost", frame, data[offset:offset+4])
		}
	}
	if got := previewCellSize(797, 159, 5, 5); got != 159.4 {
		t.Fatalf("fractional natural cell = %g; want159.4", got)
	}
	if got := previewCellSize(160, 159, 5, 1); got != 160 {
		t.Fatalf("truncated final sheet cell = %g; want160", got)
	}
	board := handler.source.Preview.Storyboard
	board.Width = 161
	if _, _, err := handler.render(context.Background(), previewRequest{}); err == nil {
		t.Fatal("undersized storyboard sheet was accepted")
	}
}

func TestDecodedPreviewCacheRetainsOnlyLatestAndClearsOnClose(t *testing.T) {
	cache := newPreviewCache(4)
	first := image.NewRGBA(image.Rect(0, 0, 1, 1))
	second := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if err := cache.retainDecoded(context.Background(), "first", first); err != nil {
		t.Fatal(err)
	}
	if err := cache.retainDecoded(context.Background(), "second", second); err != nil {
		t.Fatal(err)
	}
	if got, err := cache.decoded("first"); got != nil || err != nil {
		t.Fatalf("old decoded source = %v, error = %v", got, err)
	}
	if got, err := cache.decoded("second"); got != second || err != nil {
		t.Fatalf("latest decoded source = %v, error = %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cache.retainDecoded(ctx, "canceled", first); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled decoded source retention error = %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if cache.image != nil || cache.imageKey != "" {
		t.Fatal("close without rendered pages retained the decoded source")
	}
	if err := cache.retainDecoded(context.Background(), "closed", first); err == nil {
		t.Fatal("closed cache retained a decoded source")
	}
}

func TestPreviewCacheBoundsStorageAndSupportsRecoveryAndCancellation(t *testing.T) {
	cache := newPreviewCache(8)
	defer cache.Close()
	loads := 0
	load := func() ([]byte, previewMetadata, error) { loads++; return []byte{1, 2, 3, 4}, previewMetadata{}, nil }
	first, err := cache.get(context.Background(), "first", load)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.get(context.Background(), "second", load); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.get(context.Background(), "third", load); err != nil {
		t.Fatal(err)
	}
	if cache.bytes > 8 || loads != 3 {
		t.Fatalf("bytes = %d, loads = %d", cache.bytes, loads)
	}
	if _, err := os.Stat(first.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("evicted file stat error = %v; want os.ErrNotExist", err)
	}
	recovered, err := cache.get(context.Background(), "first", load)
	if err != nil || recovered.Path == first.Path || loads != 4 {
		t.Fatalf("recovery = %#v, error = %v, loads = %d", recovered, err, loads)
	}
	if err := os.Remove(recovered.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.get(context.Background(), "first", load); err != nil || loads != 5 {
		t.Fatalf("missing file recovery error = %v, loads = %d", err, loads)
	}
	if _, err := cache.get(context.Background(), "large", func() ([]byte, previewMetadata, error) { return make([]byte, 9), previewMetadata{}, nil }); err == nil {
		t.Fatal("oversized preview page was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.get(ctx, "cancel", func() ([]byte, previewMetadata, error) { return nil, previewMetadata{}, ctx.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.get(context.Background(), "closed", load); err == nil {
		t.Fatal("closed cache accepted a page")
	}
}

func TestPreviewCacheSharesGenerationAndCancelsWaiters(t *testing.T) {
	cache := newPreviewCache(32)
	defer cache.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	var loads atomic.Int32
	load := func() ([]byte, previewMetadata, error) {
		loads.Add(1)
		close(started)
		<-release
		return []byte{1, 2, 3, 4}, previewMetadata{}, nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := cache.get(context.Background(), "page", load); err != nil {
			t.Error(err)
		}
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := cache.get(ctx, "other", load); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := cache.get(context.Background(), "page", load); err != nil {
			t.Error(err)
		}
	}()
	close(release)
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("loads = %d", loads.Load())
	}
	files, err := filepath.Glob(filepath.Join(cache.directory, "*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("cache files = %v, error = %v; want one file and no error", files, err)
	}
}

func TestPreviewCacheRejectsCanceledMissBeforeLoading(t *testing.T) {
	cache := newPreviewCache(4)
	defer cache.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loads := 0
	for range 1000 {
		_, err := cache.get(ctx, "miss", func() ([]byte, previewMetadata, error) {
			loads++
			return nil, previewMetadata{}, ctx.Err()
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled miss error = %v", err)
		}
	}
	if loads != 0 {
		t.Fatalf("loader calls for canceled cache misses = %d; want 0", loads)
	}
}

func TestPreviewFailureAndCancellationLeaveNoCacheFiles(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid image", true: "canceled request"}[blocked], func(t *testing.T) {
			cache := newPreviewCache(previewCacheLimit)
			defer cache.Close()
			started := make(chan struct{})
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				close(started)
				if blocked {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("invalid image")), Header: make(http.Header)}, nil
			})}
			handler := &previewHandler{client: client, cache: cache, source: Resource{Name: "preview", Preview: &PreviewSource{ArtworkURLs: []string{"https://example.com/artwork"}}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, "http://local/preview", nil).WithContext(ctx)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { handler.ServeHTTP(response, request); close(done) }()
			<-started
			if blocked {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("preview request did not stop")
			}
			if !blocked && response.Code != http.StatusBadGateway {
				t.Fatalf("status = %d", response.Code)
			}
			if cache.bytes != 0 || cache.directory != "" || len(cache.entries) != 0 {
				t.Fatal("failed request published a cache page")
			}
		})
	}
}

func TestCompactPreviewImageMatchesFullRGBA(t *testing.T) {
	ratios := []image.YCbCrSubsampleRatio{image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio422, image.YCbCrSubsampleRatio420, image.YCbCrSubsampleRatio440, image.YCbCrSubsampleRatio411, image.YCbCrSubsampleRatio410}
	for _, ratio := range ratios {
		for _, origin := range []image.Point{{}, {11, 17}, {-11, -17}} {
			bounds := image.Rectangle{Min: origin, Max: origin.Add(image.Pt(1024, 768))}
			source := image.NewYCbCr(bounds, ratio)
			for index := range source.Y {
				source.Y[index] = byte(index*47 + 19)
			}
			for index := range source.Cb {
				source.Cb[index], source.Cr[index] = byte(index*29+73), byte(index*37+43)
			}
			geometry := previewLayout(bounds.Dx(), bounds.Dy(), 0, 0, true)
			width, height := geometry.width, geometry.height
			xs, ys := previewContentSamples(bounds.Dx(), width, geometry.scaledWidth), previewContentSamples(bounds.Dy(), height, geometry.scaledHeight)
			mask := previewMask(geometry.displayWidth, geometry.displayHeight)
			full := image.NewRGBA(bounds)
			draw.Draw(full, full.Bounds(), source, bounds.Min, draw.Src)
			want := make([]byte, width*height*4)
			if err := renderPreview(context.Background(), want, full, full.Bounds(), xs, ys, mask); err != nil {
				t.Fatal(err)
			}
			compact, compactXs, compactYs, err := compactPreviewImage(context.Background(), source, xs, ys)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if err := renderPreview(context.Background(), got, compact, compact.Bounds(), compactXs, compactYs, mask); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				for index := range got {
					if got[index] != want[index] {
						t.Fatalf("ratio = %v, origin = %v, byte %d = %d; want %d", ratio, origin, index, got[index], want[index])
					}
				}
			}
		}
	}
}

func TestArtworkPreviewMatchesFullImageForEncodedFormats(t *testing.T) {
	canvas := image.NewNRGBA(image.Rect(0, 0, 768, 512))
	for y := 0; y < canvas.Bounds().Dy(); y++ {
		for x := 0; x < canvas.Bounds().Dx(); x++ {
			canvas.SetNRGBA(x, y, color.NRGBA{R: byte(x), G: byte(y), B: byte(x ^ y), A: byte(x + y)})
		}
	}
	for _, format := range []string{"jpeg", "gray-jpeg", "png", "gif"} {
		t.Run(format, func(t *testing.T) {
			var buffer bytes.Buffer
			var err error
			switch format {
			case "jpeg":
				err = jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 80})
			case "gray-jpeg":
				gray := image.NewGray(canvas.Bounds())
				draw.Draw(gray, gray.Bounds(), canvas, canvas.Bounds().Min, draw.Src)
				err = jpeg.Encode(&buffer, gray, &jpeg.Options{Quality: 80})
			case "png":
				err = png.Encode(&buffer, canvas)
			case "gif":
				err = gif.Encode(&buffer, canvas, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			body := buffer.Bytes()
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header), ContentLength: int64(len(body))}, nil
			})}
			artwork := &previewHandler{client: client, source: Resource{Preview: &PreviewSource{ArtworkURLs: []string{"https://example.com/image"}}}}
			got, gotMetadata, err := artwork.render(context.Background(), previewRequest{})
			if err != nil {
				t.Fatal(err)
			}
			decoded, _, err := image.Decode(bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			full := image.NewRGBA(decoded.Bounds())
			draw.Draw(full, full.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
			geometry := previewLayout(full.Bounds().Dx(), full.Bounds().Dy(), 0, 0, false)
			want := make([]byte, geometry.width*geometry.height*4)
			xs := previewContentSamples(full.Bounds().Dx(), geometry.width, geometry.scaledWidth)
			ys := previewContentSamples(full.Bounds().Dy(), geometry.height, geometry.scaledHeight)
			if err := renderPreview(context.Background(), want, full, full.Bounds(), xs, ys, previewMask(geometry.displayWidth, geometry.displayHeight)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) || gotMetadata.Width != geometry.width || gotMetadata.Height != geometry.height {
				t.Fatalf("%s artwork preview differs from full-image sampling", format)
			}
		})
	}
}

func TestCompactPreviewImageCancelsBeforeSampling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := image.NewYCbCr(image.Rect(0, 0, 512, 512), image.YCbCrSubsampleRatio420)
	_, _, _, err := compactPreviewImage(ctx, source, previewContentSamples(512, 240, 240), previewContentSamples(512, 240, 240))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled compact image error = %v", err)
	}
}

func benchmarkPreviewRender(b *testing.B, width, height int, board bool) {
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			canvas.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	var payload bytes.Buffer
	if err := jpeg.Encode(&payload, canvas, &jpeg.Options{Quality: 80}); err != nil {
		b.Fatal(err)
	}
	body := payload.Bytes()
	source := &PreviewSource{ArtworkURLs: []string{"https://example.com/artwork.jpg"}}
	if board {
		source = &PreviewSource{Storyboard: &media.Storyboard{URL: "https://example.com/0.jpg", Width: width / 5, Height: height / 5, Columns: 5, Rows: 5, Count: 25, Interval: 2}}
	}
	handler := &previewHandler{source: Resource{Preview: source}, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header), ContentLength: int64(len(body))}, nil
	})}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := handler.render(context.Background(), previewRequest{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPreviewRender(b *testing.B) {
	for _, sample := range []struct {
		name          string
		width, height int
		storyboard    bool
	}{
		{"artwork320", 320, 180, false},
		{"artwork1280", 1280, 720, false},
		{"artwork3840", 3840, 2160, false},
		{"storyboard800", 800, 450, true},
	} {
		b.Run(sample.name, func(b *testing.B) {
			benchmarkPreviewRender(b, sample.width, sample.height, sample.storyboard)
		})
	}
}
