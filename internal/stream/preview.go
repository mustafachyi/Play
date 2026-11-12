package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"play/internal/media"
)

const (
	previewCacheLimit = 32 << 20
	previewEntryLimit = 256
	maxPreviewPixels  = 8 << 20
	previewTimeout    = 4 * time.Second
)

type PreviewSource struct {
	Storyboard  *media.Storyboard
	ArtworkURLs []string
}

type previewMetadata struct {
	Path          string  `json:"path"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	Interval      float64 `json:"interval"`
	Count         int     `json:"count"`
	Page          int     `json:"page"`
	FramesPerPage int     `json:"framesPerPage"`
	DisplayWidth  float64 `json:"displayWidth"`
	DisplayHeight float64 `json:"displayHeight"`
	MaxWidth      int     `json:"maxWidth"`
	MaxHeight     int     `json:"maxHeight"`
}

type previewRequest struct {
	page      int
	maxWidth  int
	maxHeight int
}

type previewGeometry struct {
	width         int
	height        int
	displayWidth  float64
	displayHeight float64
	scaledWidth   float64
	scaledHeight  float64
}

type previewHandler struct {
	client *http.Client
	source Resource
	cache  *previewCache
}

func validatePreview(source *PreviewSource) error {
	if source == nil || (source.Storyboard == nil) == (len(source.ArtworkURLs) == 0) {
		return errors.New("preview source is invalid")
	}
	if board := source.Storyboard; board != nil {
		if board.Width <= 0 || board.Width > 1024 || board.Height <= 0 || board.Height > 1024 || board.Columns <= 0 || board.Rows <= 0 || board.Columns > 100 || board.Rows > 100 || board.Columns*board.Rows > 100 || board.Count <= 0 || board.Count > 1_000_000 || math.IsNaN(board.Interval) || math.IsInf(board.Interval, 0) || board.Interval <= 0 {
			return errors.New("storyboard dimensions or timing are invalid")
		}
		if board.Count > board.Columns*board.Rows && !strings.Contains(board.URL, "$M") {
			return errors.New("storyboard page pattern is missing")
		}
		return validateResourceURL(strings.ReplaceAll(board.URL, "$M", "0"))
	}
	for _, value := range source.ArtworkURLs {
		if err := validateResourceURL(value); err != nil {
			return err
		}
	}
	return nil
}

func (h *previewHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	request, err := h.request(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "preview request is invalid", http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), previewTimeout)
	defer cancel()
	key := h.source.Name + ":" + strconv.Itoa(request.page) + ":" + strconv.Itoa(request.maxWidth) + "x" + strconv.Itoa(request.maxHeight)
	metadata, err := h.cache.get(ctx, key, func() ([]byte, previewMetadata, error) {
		return h.render(ctx, request)
	})
	if err != nil {
		if r.Context().Err() == nil {
			http.Error(w, "preview unavailable", http.StatusBadGateway)
		}
		return
	}
	_ = json.NewEncoder(w).Encode(metadata)
}

func (h *previewHandler) request(raw string) (previewRequest, error) {
	query, err := url.ParseQuery(raw)
	if err != nil || len(query) > 3 || query.Has("page") && query.Has("time") || query.Has("width") != query.Has("height") {
		return previewRequest{}, errors.New("invalid preview query")
	}
	count, perPage, interval := 1, 1, float64(0)
	if board := h.source.Preview.Storyboard; board != nil {
		count, perPage, interval = board.Count, board.Columns*board.Rows, board.Interval
	}
	request := previewRequest{}
	for key, values := range query {
		if len(values) != 1 || values[0] == "" {
			return request, errors.New("invalid preview value")
		}
		switch key {
		case "page":
			request.page, err = strconv.Atoi(values[0])
			if err != nil {
				return request, err
			}
		case "time":
			seconds, parseErr := strconv.ParseFloat(values[0], 64)
			if parseErr != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
				return request, errors.New("invalid preview time")
			}
			if interval > 0 {
				frame := math.Min(float64(count-1), math.Floor(seconds/interval))
				request.page = int(frame) / perPage
			}
		case "width", "height":
			size, parseErr := strconv.ParseUint(values[0], 10, 16)
			if parseErr != nil || size < 3 || size > 512 {
				return request, errors.New("invalid preview size")
			}
			if key == "width" {
				request.maxWidth = int(size)
			} else {
				request.maxHeight = int(size)
			}
		default:
			return request, errors.New("unknown preview value")
		}
	}
	if request.page < 0 || request.page > (count-1)/perPage {
		return request, errors.New("preview page is outside the source")
	}
	return request, nil
}

func (h *previewHandler) render(ctx context.Context, request previewRequest) ([]byte, previewMetadata, error) {
	metadata := previewMetadata{Count: 1, FramesPerPage: 1, Page: request.page, MaxWidth: request.maxWidth, MaxHeight: request.maxHeight}
	var source image.Image
	var err error
	frameWidth, frameHeight, columns := 0, 0, 1
	if board := h.source.Preview.Storyboard; board != nil {
		metadata.Count, metadata.FramesPerPage, metadata.Interval = board.Count, board.Columns*board.Rows, board.Interval
		frameWidth, frameHeight, columns = board.Width, board.Height, board.Columns
		source, err = h.image(ctx, []string{strings.ReplaceAll(board.URL, "$M", strconv.Itoa(request.page))})
	} else {
		source, err = h.image(ctx, h.source.Preview.ArtworkURLs)
		if err == nil {
			frameWidth, frameHeight = source.Bounds().Dx(), source.Bounds().Dy()
		}
	}
	if err != nil {
		return nil, metadata, err
	}
	if err := ctx.Err(); err != nil {
		return nil, metadata, err
	}
	geometry := previewLayout(frameWidth, frameHeight, request.maxWidth, request.maxHeight, h.source.Preview.Storyboard != nil)
	metadata.Width, metadata.Height = geometry.width, geometry.height
	metadata.DisplayWidth, metadata.DisplayHeight = geometry.displayWidth, geometry.displayHeight
	frames := min(metadata.FramesPerPage, metadata.Count-request.page*metadata.FramesPerPage)
	frameBytes := metadata.Width * metadata.Height * 4
	if frames <= 0 || frameBytes*frames > previewCacheLimit {
		return nil, metadata, errors.New("preview page is too large")
	}
	data := make([]byte, frameBytes*frames)
	mask := previewMask(geometry.displayWidth, geometry.displayHeight)
	xSamples := previewContentSamples(frameWidth, metadata.Width, geometry.scaledWidth)
	ySamples := previewContentSamples(frameHeight, metadata.Height, geometry.scaledHeight)
	if jpeg, ok := source.(*image.YCbCr); ok && h.source.Preview.Storyboard == nil && frameWidth*frameHeight > len(xSamples)*len(ySamples)*4 {
		compact, xs, ys, err := compactPreviewImage(ctx, jpeg, xSamples, ySamples)
		if err != nil {
			return nil, metadata, err
		}
		err = renderPreview(ctx, data, compact, compact.Bounds(), xs, ys, mask)
		return data, metadata, err
	}
	imageRGBA, ok := source.(*image.RGBA)
	if !ok {
		imageRGBA = image.NewRGBA(source.Bounds())
		draw.Draw(imageRGBA, imageRGBA.Bounds(), source, source.Bounds().Min, draw.Src)
	}
	sampleWidth, sampleHeight := float64(frameWidth), float64(frameHeight)
	if board := h.source.Preview.Storyboard; board != nil {
		sampleWidth = previewCellSize(source.Bounds().Dx(), frameWidth, columns, min(columns, frames))
		sampleHeight = previewCellSize(source.Bounds().Dy(), frameHeight, board.Rows, (frames+columns-1)/columns)
	}
	for frame := 0; frame < frames; frame++ {
		x, y := float64(frame%columns)*sampleWidth, float64(frame/columns)*sampleHeight
		if x+sampleWidth > float64(source.Bounds().Dx()) || y+sampleHeight > float64(source.Bounds().Dy()) {
			return nil, metadata, errors.New("storyboard frame is outside the image")
		}
		xSamples = previewFrameSamples(source.Bounds().Dx(), sampleWidth, x, metadata.Width, geometry.scaledWidth)
		ySamples = previewFrameSamples(source.Bounds().Dy(), sampleHeight, y, metadata.Height, geometry.scaledHeight)
		if err := renderPreview(ctx, data[frame*frameBytes:(frame+1)*frameBytes], imageRGBA, imageRGBA.Bounds(), xSamples, ySamples, mask); err != nil {
			return nil, metadata, err
		}
	}
	return data, metadata, nil
}

func previewCellSize(actual, declared, count, visibleCount int) float64 {
	for _, cells := range []int{count, visibleCount} {
		if cells > 0 && actual >= declared*cells && actual <= (declared+1)*cells {
			return float64(actual) / float64(cells)
		}
	}
	return float64(declared)
}

func compactPreviewImage(ctx context.Context, source *image.YCbCr, xs, ys []previewSample) (*image.RGBA, []previewSample, []previewSample, error) {
	compactXs, columns := compactPreviewSamples(xs)
	compactYs, rows := compactPreviewSamples(ys)
	pixels := image.NewRGBA(image.Rect(0, 0, len(columns), len(rows)))
	origin := source.Bounds().Min
	for y, row := range rows {
		if y%16 == 0 && ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		for x, column := range columns {
			pixel := source.YCbCrAt(origin.X+column, origin.Y+row)
			r, g, b := color.YCbCrToRGB(pixel.Y, pixel.Cb, pixel.Cr)
			pixels.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return pixels, compactXs, compactYs, nil
}

func compactPreviewSamples(samples []previewSample) ([]previewSample, []int) {
	compact := make([]previewSample, len(samples))
	positions := make([]int, len(samples)*2)
	for index, sample := range samples {
		compact[index] = previewSample{first: index * 2, second: index*2 + 1, fraction: sample.fraction}
		positions[index*2], positions[index*2+1] = sample.first, sample.second
	}
	return compact, positions
}

func (h *previewHandler) image(ctx context.Context, sources []string) (image.Image, error) {
	key := strings.Join(sources, "\x00")
	if h.cache != nil {
		if cached, err := h.cache.decoded(key); cached != nil || err != nil {
			return cached, err
		}
	}
	lastErr := errors.New("preview image is unavailable")
	for _, source := range sources {
		body, _, err := fetchAsset(ctx, h.client, source)
		if err == nil {
			config, _, decodeErr := image.DecodeConfig(bytes.NewReader(body))
			if decodeErr != nil || config.Width <= 0 || config.Height <= 0 || config.Width > maxPreviewPixels || config.Height > maxPreviewPixels/config.Width {
				err = errors.New("preview image is unsupported or too large")
			} else {
				var decoded image.Image
				decoded, _, err = image.Decode(bytes.NewReader(body))
				if err == nil {
					if h.cache != nil {
						if err := h.cache.retainDecoded(ctx, key, decoded); err != nil {
							return nil, err
						}
					}
					return decoded, nil
				}
			}
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func previewLayout(width, height, maxWidth, maxHeight int, storyboard bool) previewGeometry {
	factor := math.Min(243/float64(width), 141/float64(height))
	scaledWidth, scaledHeight := float64(width)*factor, float64(height)*factor
	visibleWidth, visibleHeight := scaledWidth, scaledHeight
	if storyboard {
		scaledWidth, scaledHeight = math.Max(1, math.Floor(scaledWidth)), math.Max(1, math.Floor(scaledHeight))
		visibleWidth, visibleHeight = math.Max(1, scaledWidth-2/factor), scaledHeight
		if height <= 45 {
			visibleHeight = math.Max(1, visibleHeight-1/factor)
		}
	}
	fit := float64(1)
	if maxWidth > 0 && maxHeight > 0 {
		fit = math.Min(1, math.Min(float64(maxWidth-2)/visibleWidth, float64(maxHeight-2)/visibleHeight))
	}
	displayWidth := math.Round(math.Max(1, visibleWidth*fit)*60)/60 + 2
	displayHeight := math.Round(math.Max(1, visibleHeight*fit)*60)/60 + 2
	return previewGeometry{
		width: int(math.Ceil(displayWidth)), height: int(math.Ceil(displayHeight)),
		displayWidth: displayWidth, displayHeight: displayHeight,
		scaledWidth: scaledWidth * fit, scaledHeight: scaledHeight * fit,
	}
}

type previewPixel struct{ interior, border uint8 }

func previewMask(width, height float64) []previewPixel {
	canvasWidth, canvasHeight := int(math.Ceil(width)), int(math.Ceil(height))
	mask := make([]previewPixel, canvasWidth*canvasHeight)
	for y := 0; y < canvasHeight; y++ {
		for x := 0; x < canvasWidth; x++ {
			outer := roundedCoverage(x, y, width, height, 8)
			inner := roundedCoverage(x-1, y-1, width-2, height-2, 8)
			mask[y*canvasWidth+x] = previewPixel{inner, outer - inner}
		}
	}
	return mask
}

func roundedCoverage(x, y int, width, height, radius float64) uint8 {
	if width <= 0 || height <= 0 || x < 0 || y < 0 || float64(x) >= width || float64(y) >= height {
		return 0
	}
	radius = min(radius, width/2, height/2)
	if float64(x) >= radius && float64(x+1) <= width-radius || float64(y) >= radius && float64(y+1) <= height-radius {
		return uint8(math.Round(math.Min(1, width-float64(x)) * math.Min(1, height-float64(y)) * 255))
	}
	inside := 0
	for sy := 0; sy < 4; sy++ {
		for sx := 0; sx < 4; sx++ {
			px, py := float64(x)+(float64(sx)+.5)/4, float64(y)+(float64(sy)+.5)/4
			cx, cy := max(radius, min(width-radius, px)), max(radius, min(height-radius, py))
			if px < width && py < height && (px-cx)*(px-cx)+(py-cy)*(py-cy) <= radius*radius {
				inside++
			}
		}
	}
	return uint8((inside*255 + 8) / 16)
}

type previewSample struct{ first, second, fraction int }

func previewContentSamples(size, output int, scaled float64) []previewSample {
	return previewFrameSamples(size, float64(size), 0, output, scaled)
}

func previewFrameSamples(size int, frameSize, offset float64, output int, scaled float64) []previewSample {
	values := make([]previewSample, output)
	for position := range values {
		value := max(0, min(float64(size-1), (float64(position-1)+.5)*frameSize/scaled-.5+offset))
		first := int(value)
		values[position] = previewSample{first, min(first+1, size-1), int(math.Round((value - float64(first)) * 256))}
	}
	return values
}

func renderPreview(ctx context.Context, output []byte, source *image.RGBA, rectangle image.Rectangle, xs, ys []previewSample, mask []previewPixel) error {
	width := len(xs)
	channels := [4]int{2, 1, 0, 3}
	for y, sy := range ys {
		if y%16 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		for x, sx := range xs {
			first := source.PixOffset(rectangle.Min.X+sx.first, rectangle.Min.Y+sy.first)
			second := source.PixOffset(rectangle.Min.X+sx.second, rectangle.Min.Y+sy.first)
			third := source.PixOffset(rectangle.Min.X+sx.first, rectangle.Min.Y+sy.second)
			fourth := source.PixOffset(rectangle.Min.X+sx.second, rectangle.Min.Y+sy.second)
			index := y*width + x
			pixel := mask[index]
			for channel, sourceChannel := range channels {
				top := int(source.Pix[first+sourceChannel])*(256-sx.fraction) + int(source.Pix[second+sourceChannel])*sx.fraction
				bottom := int(source.Pix[third+sourceChannel])*(256-sx.fraction) + int(source.Pix[fourth+sourceChannel])*sx.fraction
				value := (top*(256-sy.fraction) + bottom*sy.fraction + 32768) >> 16
				output[index*4+channel] = uint8((value*int(pixel.interior) + 255*int(pixel.border) + 127) / 255)
			}
		}
	}
	return nil
}
