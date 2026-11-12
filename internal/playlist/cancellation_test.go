package playlist

import (
	"context"
	"errors"
	"strings"
	"testing"

	"play/internal/media"
	"play/internal/stream"
)

func TestCanceledResolutionPreservesRegisteredStreams(t *testing.T) {
	gateway, err := stream.StartDynamic(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolutions := 0
	server, err := Start([]media.PlaylistItem{{VideoID: "aaaaaaaaaaa"}}, func(context.Context, int, string) (ResolvedItem, error) {
		resolutions++
		if err := gateway.Add([]stream.Resource{{Name: "p0001-a01.webm", URL: "https://example.com/audio", MIME: "audio/webm", Size: 8, Ranged: true}}); err != nil {
			return ResolvedItem{}, err
		}
		cancel()
		return ResolvedItem{EDL: "# mpv EDL v0\n" + gateway.URL("p0001-a01.webm") + "\n"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.Prepare(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned resolution error = %v; want context.Canceled", err)
	}
	if err := server.Prepare(context.Background(), 0); err != nil {
		t.Fatalf("retry discarded committed gateway resource: %v", err)
	}
	if resolutions != 1 {
		t.Fatalf("resolver calls = %d; want committed result reuse without duplicate Add", resolutions)
	}
}

func TestCanceledPageLoadPreservesContinuation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pages := 0
	server, err := Start([]media.PlaylistItem{{VideoID: "aaaaaaaaaaa", Title: "A"}}, func(context.Context, int, string) (ResolvedItem, error) {
		return ResolvedItem{EDL: "# mpv EDL v0\n"}, nil
	}, func(context.Context) ([]media.PlaylistItem, bool, error) {
		pages++
		if pages == 1 {
			cancel()
			return []media.PlaylistItem{{VideoID: "bbbbbbbbbbb", Title: "B"}}, true, nil
		}
		return []media.PlaylistItem{{VideoID: "ccccccccccc", Title: "C"}}, false, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if _, err := server.loadPage(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned page error = %v; want context.Canceled", err)
	}
	body, err := server.loadPage(context.Background(), 2)
	if err != nil || !strings.Contains(body, "#EXTINF:-1,B\n") || strings.Contains(body, "#EXTINF:-1,C\n") || pages != 1 {
		t.Fatalf("retry skipped committed producer page: body = %q, calls = %d, error = %v", body, pages, err)
	}
	body, err = server.loadPage(context.Background(), 3)
	if err != nil || !strings.Contains(body, "#EXTINF:-1,C\n") || pages != 2 {
		t.Fatalf("following page = %q, calls = %d, error = %v", body, pages, err)
	}
	if len(server.entries) != 3 || server.entries[1].item.VideoID != "bbbbbbbbbbb" || server.entries[2].item.VideoID != "ccccccccccc" {
		t.Fatalf("committed playlist entries = %#v", server.entries)
	}
}
