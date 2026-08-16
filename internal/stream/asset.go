package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	assetRequestTimeout       = 30 * time.Second
	maxAssetSize        int64 = 16 << 20
)

type assetHandler struct {
	client   *http.Client
	source   Resource
	reporter Reporter
}

func (h *assetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if h.source.MIME != "" {
		w.Header().Set("Content-Type", h.source.MIME)
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	body, contentType, err := fetchAsset(r.Context(), h.client, h.source.URL)
	if err != nil {
		if h.reporter != nil && r.Context().Err() == nil {
			h.reporter(h.source.Name, err)
		}
		http.Error(w, "asset unavailable", http.StatusBadGateway)
		return
	}
	if h.source.MIME == "" && contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func fetchAsset(ctx context.Context, client *http.Client, sourceURL string) ([]byte, string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxChunkAttempts; attempt++ {
		body, contentType, retryable, err := fetchAssetOnce(ctx, client, sourceURL)
		if err == nil {
			return body, contentType, nil
		}
		lastErr = err
		if !retryable || attempt == maxChunkAttempts {
			break
		}
		if err := waitRetry(ctx, attempt); err != nil {
			return nil, "", err
		}
	}
	return nil, "", lastErr
}

func fetchAssetOnce(ctx context.Context, client *http.Client, sourceURL string) ([]byte, string, bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, assetRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, "", false, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", false, ctx.Err()
		}
		if requestCtx.Err() == context.DeadlineExceeded {
			return nil, "", true, errors.New("upstream asset request timed out")
		}
		return nil, "", true, fmt.Errorf("upstream asset request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		return nil, "", retryableStatus(resp.StatusCode), fmt.Errorf("upstream asset returned HTTP %d", resp.StatusCode)
	}
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, "", false, errors.New("upstream asset returned encoded bytes")
	}
	if resp.ContentLength > maxAssetSize {
		return nil, "", false, errors.New("upstream asset is too large")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetSize+1))
	if err != nil {
		return nil, "", true, fmt.Errorf("read upstream asset: %w", err)
	}
	if int64(len(body)) > maxAssetSize {
		return nil, "", false, errors.New("upstream asset is too large")
	}
	return body, strings.TrimSpace(resp.Header.Get("Content-Type")), false, nil
}
