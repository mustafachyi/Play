package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const previewTestPath = "/0123456789abcdef0123456789abcdef/preview.json"

func TestPreviewHelper(t *testing.T) {
	payload := `{"path":"frame.bgra","width":240,"height":135}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != previewTestPath || r.URL.Query().Get("time") != "12" {
			t.Errorf("unexpected preview request: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, payload)
	}))
	defer server.Close()
	var output strings.Builder
	if err := Run(context.Background(), "test", []string{"-preview", server.URL + previewTestPath + "?time=12"}, nil, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if output.String() != payload {
		t.Fatalf("preview output = %q; want the response payload unchanged", output.String())
	}
}

func TestPreviewHelperRejectsInvalidRequests(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"https://example.com/preview.json"},
		{"http://127.0.0.1/preview.json"},
		{"http://127.0.0.1:8000/not-a-token/preview.json"},
		{"http://127.0.0.1:8000" + previewTestPath + "#fragment"},
		{"http://user@127.0.0.1:8000" + previewTestPath},
		{"http://127.0.0.1:8000" + previewTestPath, "extra"},
	} {
		if err := fetchPreview(context.Background(), args, io.Discard); err == nil {
			t.Errorf("invalid preview request was accepted: %v", args)
		}
	}
}

func TestPreviewHelperRejectsInvalidResponses(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"unavailable", http.StatusServiceUnavailable, "unavailable"},
		{"invalid-json", http.StatusOK, "invalid"},
		{"oversized", http.StatusOK, `"` + strings.Repeat("x", 8192) + `"`},
		{"redirect", http.StatusFound, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.status == http.StatusFound {
					w.Header().Set("Location", "https://example.com")
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			var output strings.Builder
			if err := fetchPreview(context.Background(), []string{server.URL + previewTestPath}, &output); err == nil || output.Len() != 0 {
				t.Fatalf("error = %v, output = %q; want a rejected response and empty output", err, output.String())
			}
		})
	}
}

func TestPreviewHelperPreservesCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := fetchPreview(ctx, []string{server.URL + previewTestPath}, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
}
