package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxChunkSize        int64 = 9_750_000
	maxChunkAttempts          = 3
	rangeRequestTimeout       = 2 * time.Minute
)

type byteRange struct {
	start   int64
	end     int64
	partial bool
}

type sourceHandler struct {
	client    *http.Client
	source    Resource
	chunkSize int64
	reporter  Reporter
	sizeMu    sync.Mutex
	size      int64
}

type trackingWriter struct {
	writer io.Writer
	err    error
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}
	return n, err
}

func (h *sourceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	size, err := h.contentSize(r.Context())
	if err != nil {
		h.report(r.Context(), fmt.Errorf("determine media size: %w", err))
		http.Error(w, "media unavailable", http.StatusBadGateway)
		return
	}
	requested, err := parseRange(r.Header.Get("Range"), size)
	if err != nil {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	length := requested.end - requested.start + 1
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if h.source.MIME != "" {
		w.Header().Set("Content-Type", h.source.MIME)
	}
	status := http.StatusOK
	if requested.partial {
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requested.start, requested.end, size))
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if err := h.copySpan(r.Context(), w, requested.start, requested.end, size); err != nil {
		h.report(r.Context(), err)
		panic(http.ErrAbortHandler)
	}
}

func (h *sourceHandler) contentSize(ctx context.Context) (int64, error) {
	if h.source.Size > 0 {
		return h.source.Size, nil
	}
	h.sizeMu.Lock()
	defer h.sizeMu.Unlock()
	if h.size > 0 {
		return h.size, nil
	}
	size, err := probeSize(ctx, h.client, h.source.URL)
	if err != nil {
		return 0, err
	}
	h.size = size
	return size, nil
}

func (h *sourceHandler) copySpan(ctx context.Context, dst io.Writer, start, end, total int64) error {
	chunkSize := h.chunkSize
	if chunkSize <= 0 || chunkSize > maxChunkSize {
		chunkSize = maxChunkSize
	}
	position := start
	for position <= end {
		chunkEnd := position + chunkSize - 1
		if chunkEnd < position || chunkEnd > end {
			chunkEnd = end
		}
		attempts := 0
		var lastErr error
		for position <= chunkEnd {
			if err := ctx.Err(); err != nil {
				return err
			}
			if attempts >= maxChunkAttempts {
				return fmt.Errorf("upstream range request failed after %d attempts: %w", maxChunkAttempts, lastErr)
			}
			attempts++
			partStart := position
			written, retryable, err := h.fetchPart(ctx, dst, partStart, chunkEnd, total)
			position += written
			if err == nil {
				break
			}
			lastErr = fmt.Errorf("bytes %d-%d: %w", partStart, chunkEnd, err)
			if !retryable {
				return lastErr
			}
			if attempts < maxChunkAttempts {
				if err := waitRetry(ctx, attempts); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (h *sourceHandler) fetchPart(ctx context.Context, dst io.Writer, start, end, total int64) (int64, bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, rangeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, h.source.URL, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := h.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, false, ctx.Err()
		}
		if requestCtx.Err() == context.DeadlineExceeded {
			return 0, true, errors.New("upstream range request timed out")
		}
		return 0, true, fmt.Errorf("upstream range request failed: %w", err)
	}
	expected, err := validateRangeResponse(resp, start, end, total)
	if err != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		return 0, retryableStatus(resp.StatusCode), err
	}
	writer := &trackingWriter{writer: dst}
	written, copyErr := io.CopyN(writer, resp.Body, expected)
	closeErr := resp.Body.Close()
	if writer.err != nil {
		return written, false, writer.err
	}
	if copyErr != nil {
		return written, true, fmt.Errorf("read upstream range: %w", copyErr)
	}
	if closeErr != nil {
		return written, true, fmt.Errorf("close upstream range: %w", closeErr)
	}
	return written, false, nil
}

func (h *sourceHandler) report(ctx context.Context, err error) {
	if h.reporter == nil || err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	h.reporter(h.source.Name, err)
}

func probeSize(ctx context.Context, client *http.Client, sourceURL string) (int64, error) {
	var lastErr error
	for attempt := 1; attempt <= maxChunkAttempts; attempt++ {
		size, retryable, err := probeSizeOnce(ctx, client, sourceURL)
		if err == nil {
			return size, nil
		}
		lastErr = err
		if !retryable || attempt == maxChunkAttempts {
			break
		}
		if err := waitRetry(ctx, attempt); err != nil {
			return 0, err
		}
	}
	return 0, lastErr
}

func probeSizeOnce(ctx context.Context, client *http.Client, sourceURL string) (int64, bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, assetRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, false, ctx.Err()
		}
		if requestCtx.Err() == context.DeadlineExceeded {
			return 0, true, errors.New("upstream size probe timed out")
		}
		return 0, true, fmt.Errorf("upstream size probe failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		return 0, retryableStatus(resp.StatusCode), fmt.Errorf("upstream size probe returned HTTP %d", resp.StatusCode)
	}
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return 0, false, errors.New("upstream size probe returned encoded bytes")
	}
	start, end, total, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil || start != 0 || end != 0 || total <= 0 {
		return 0, false, errors.New("upstream returned an invalid size probe response")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2))
	return total, false, nil
}

func waitRetry(ctx context.Context, attempt int) error {
	delay := 150 * time.Millisecond
	if attempt > 1 {
		delay = 400 * time.Millisecond
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateRangeResponse(resp *http.Response, start, end, total int64) (int64, error) {
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return 0, errors.New("upstream returned encoded media bytes")
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("upstream range request returned HTTP %d", resp.StatusCode)
	}
	gotStart, gotEnd, gotTotal, err := parseContentRange(resp.Header.Get("Content-Range"))
	if err != nil || gotStart != start || gotEnd < start || gotEnd > end || gotTotal != total {
		return 0, errors.New("upstream returned an invalid content range")
	}
	length := gotEnd - gotStart + 1
	if resp.ContentLength >= 0 && resp.ContentLength != length {
		return 0, errors.New("upstream returned an unexpected range length")
	}
	return length, nil
}

func parseRange(value string, size int64) (byteRange, error) {
	if size <= 0 {
		return byteRange{}, errors.New("invalid resource size")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return byteRange{start: 0, end: size - 1}, nil
	}
	if len(value) < 6 || !strings.EqualFold(value[:6], "bytes=") || strings.Contains(value[6:], ",") {
		return byteRange{}, errors.New("unsupported range")
	}
	spec := strings.TrimSpace(value[6:])
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return byteRange{}, errors.New("invalid range")
	}
	left, right := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	if left == "" {
		suffix, err := parseNonNegativeInt(right)
		if err != nil || suffix <= 0 {
			return byteRange{}, errors.New("invalid suffix range")
		}
		if suffix > size {
			suffix = size
		}
		return byteRange{start: size - suffix, end: size - 1, partial: true}, nil
	}
	start, err := parseNonNegativeInt(left)
	if err != nil || start >= size {
		return byteRange{}, errors.New("range start is outside the resource")
	}
	end := size - 1
	if right != "" {
		end, err = parseNonNegativeInt(right)
		if err != nil || end < start {
			return byteRange{}, errors.New("invalid range end")
		}
		if end >= size {
			end = size - 1
		}
	}
	return byteRange{start: start, end: end, partial: true}, nil
}

func parseContentRange(value string) (int64, int64, int64, error) {
	value = strings.TrimSpace(value)
	if len(value) < 7 || !strings.EqualFold(value[:6], "bytes ") {
		return 0, 0, 0, errors.New("invalid content range")
	}
	parts := strings.SplitN(strings.TrimSpace(value[6:]), "/", 2)
	if len(parts) != 2 || parts[1] == "*" {
		return 0, 0, 0, errors.New("invalid content range total")
	}
	rangeParts := strings.SplitN(parts[0], "-", 2)
	if len(rangeParts) != 2 {
		return 0, 0, 0, errors.New("invalid content range bounds")
	}
	start, err := parseNonNegativeInt(strings.TrimSpace(rangeParts[0]))
	if err != nil {
		return 0, 0, 0, err
	}
	end, err := parseNonNegativeInt(strings.TrimSpace(rangeParts[1]))
	if err != nil {
		return 0, 0, 0, err
	}
	total, err := parseNonNegativeInt(strings.TrimSpace(parts[1]))
	if err != nil || total <= 0 || start > end || end >= total {
		return 0, 0, 0, errors.New("invalid content range values")
	}
	return start, end, total, nil
}

func parseNonNegativeInt(value string) (int64, error) {
	if value == "" {
		return 0, errors.New("empty integer")
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid integer")
		}
	}
	return strconv.ParseInt(value, 10, 64)
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status == http.StatusInternalServerError || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}
