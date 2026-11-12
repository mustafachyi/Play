package app

import (
	"testing"

	"play/internal/media"
)

func TestPreviewFollowsPlaybackPlanAndPlaylistNamespace(t *testing.T) {
	item := testItem()
	item.Storyboard = &media.Storyboard{URL: "https://i.ytimg.com/page$M.jpg", Width: 160, Height: 90, Columns: 5, Rows: 5, Count: 108, Interval: 2}
	for _, audioOnly := range []bool{false, true} {
		plan, err := makePlaybackPlan(item, audioOnly)
		if err != nil {
			t.Fatal(err)
		}
		namespacePlaybackPlan(&plan, 2)
		request, err := playbackRequest(item, 0, plan, gateway{url: func(name string) string { return "http://127.0.0.1:1234/token/" + name }})
		if err != nil {
			t.Fatal(err)
		}
		if request.PreviewURL != "http://127.0.0.1:1234/token/p0003-preview.json" {
			t.Fatalf("preview URL = %q; want %q", request.PreviewURL, "http://127.0.0.1:1234/token/p0003-preview.json")
		}
		resource := plan.resources[len(plan.resources)-1]
		if resource.Name != "p0003-preview.json" || resource.Preview == nil || (resource.Preview.Storyboard == nil) != audioOnly {
			t.Fatalf("resource = %#v", resource)
		}
	}
	item.Storyboard = nil
	plan, err := makePlaybackPlan(item, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.preview != "" {
		t.Fatal("video without storyboards exposed a preview")
	}
}
