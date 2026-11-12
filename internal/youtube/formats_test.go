package youtube

import "testing"

func TestExtractionRejectsHTTPSAuthoritiesWithoutAHostname(t *testing.T) {
	for _, address := range []string{"https://:", "https://:443", "https://:443/path"} {
		t.Run(address, func(t *testing.T) {
			if validHTTPS(address) {
				t.Fatalf("accepted invalid stream address %q", address)
			}
			if got := webVTTURL(address); got != "" {
				t.Fatalf("accepted invalid caption address %q", got)
			}
			response := playerResponse{VideoDetails: &videoDetails{Thumbnail: &thumbnailList{Thumbnails: []thumbnail{{URL: address}}}}}
			if got := chooseThumbnails([]playerResponse{response}, ""); len(got) != 0 {
				t.Fatalf("accepted invalid thumbnail addresses %#v", got)
			}
			spec := address + "|160#90#20#5#5#1000#default#signature"
			if got := parseStoryboard(encodeStoryboard(t, spec, 0), 20); got != nil {
				t.Fatalf("accepted invalid storyboard %#v", got)
			}
		})
	}
}

func TestMalformedBestStreamDoesNotDisplaceUsableFallback(t *testing.T) {
	formats := []format{
		{URL: "https://:443/bad", MIMEType: "video/mp4", Height: flexibleInt{value: 720, set: true}, Bitrate: flexibleInt{value: 200, set: true}},
		{URL: "https://video.example/usable", MIMEType: "video/mp4", Height: flexibleInt{value: 720, set: true}, Bitrate: flexibleInt{value: 100, set: true}},
		{URL: "https://:443/bad", MIMEType: "audio/mp4", Bitrate: flexibleInt{value: 200, set: true}},
		{URL: "https://audio.example/usable", MIMEType: "audio/mp4", Bitrate: flexibleInt{value: 100, set: true}},
	}
	videos, audios := chooseVideos(formats), chooseAudios(formats)
	if len(videos) != 1 || videos[0].URL != "https://video.example/usable" {
		t.Fatalf("videos = %#v", videos)
	}
	if len(audios) != 1 || audios[0].URL != "https://audio.example/usable" {
		t.Fatalf("audios = %#v", audios)
	}
}

func TestDRMClassificationAcceptsWhitespaceInsideEmptyArrays(t *testing.T) {
	for _, value := range []string{"", "null", "[]", "[ ]", "[\r\n\t ]", " \n[ \t ]\r\n"} {
		if hasDRM([]byte(value)) {
			t.Errorf("empty DRM families rejected: %q", value)
		}
	}
	for _, value := range []string{`["widevine"]`, "[null]", "{}", "false", "0", "invalid"} {
		if !hasDRM([]byte(value)) {
			t.Errorf("protected or invalid DRM families accepted: %q", value)
		}
	}
}
