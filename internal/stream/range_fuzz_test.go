package stream

import (
	"fmt"
	"math"
	"testing"
)

func FuzzHTTPRangeBounds(f *testing.F) {
	for _, seed := range []struct {
		value string
		size  int64
	}{
		{"", 1},
		{"bytes=0-0", 1},
		{"bytes=1-", 2},
		{"bytes=-1", math.MaxInt64},
		{"bytes=9223372036854775806-9223372036854775807", math.MaxInt64},
		{"bytes=0-1,4-5", 8},
		{"bytes=+1-2", 8},
		{"bytes=-0", 0},
		{"bytes=0-0", -1},
		{"bytes=\x000-1", 2},
		{"", 100},
		{"bytes=0-0", 100},
		{"bytes=-1", 100},
		{"bytes=1-", 100},
		{"bytes=9223372036854775807-", 100},
		{"bytes=0-9223372036854775807", 100},
		{"bytes=-9223372036854775808", 100},
		{"bytes=1-2,4-5", 100},
	} {
		f.Add(seed.value, seed.size)
	}
	f.Fuzz(func(t *testing.T, value string, size int64) {
		span, err := parseRange(value, size)
		if err != nil {
			return
		}
		if size <= 0 || span.start < 0 || span.start > span.end || span.end >= size {
			t.Fatalf("range %q for size %d produced invalid bounds %+v", value, size, span)
		}
		canonical := fmt.Sprintf("bytes=%d-%d", span.start, span.end)
		again, err := parseRange(canonical, size)
		if err != nil || again.start != span.start || again.end != span.end || !again.partial {
			t.Fatalf("range %q did not preserve bounds: %+v, %v", canonical, again, err)
		}
	})
}

func FuzzHTTPContentRangeBounds(f *testing.F) {
	for _, value := range []string{
		"bytes 0-0/1",
		"bytes 0-7/8",
		"bytes 9223372036854775806-9223372036854775806/9223372036854775807",
		"bytes */0",
		"bytes 0-0/*",
		"bytes 1-0/2",
		"bytes -1-0/2",
		"bytes 0-1/9223372036854775808",
		"bytes 0-1/2\x00",
	} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		start, end, total, err := parseContentRange(value)
		if err != nil {
			return
		}
		if total <= 0 || start < 0 || start > end || end >= total {
			t.Fatalf("content range %q produced invalid bounds %d-%d/%d", value, start, end, total)
		}
		span, err := parseRange(fmt.Sprintf("bytes=%d-%d", start, end), total)
		if err != nil || span.start != start || span.end != end {
			t.Fatalf("content range %q cannot be served consistently: %+v, %v", value, span, err)
		}
	})
}
