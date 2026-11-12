package stream

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type chunkDigestWriter struct {
	hash  hash.Hash
	bytes int64
}

func (w *chunkDigestWriter) Write(p []byte) (int, error) {
	n, err := w.hash.Write(p)
	w.bytes += int64(n)
	return n, err
}

func writeChunkPattern(dst io.Writer, start, length int64) error {
	buffer := make([]byte, 32<<10)
	for length > 0 {
		count := min(int64(len(buffer)), length)
		for index := range buffer[:count] {
			buffer[index] = byte((start + int64(index)) % 251)
		}
		written, err := dst.Write(buffer[:count])
		if err != nil {
			return err
		}
		if int64(written) != count {
			return io.ErrShortWrite
		}
		start += count
		length -= count
	}
	return nil
}

func TestSourceHandlerDefaultChunksResumeAcrossBoundaries(t *testing.T) {
	const start int64 = 37
	length := 2*maxChunkSize + 17
	end := start + length - 1
	total := end + 100
	var calls atomic.Int32
	var mutex sync.Mutex
	var ranges []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		mutex.Lock()
		ranges = append(ranges, rangeHeader)
		mutex.Unlock()
		var rangeStart, rangeEnd int64
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &rangeStart, &rangeEnd); err != nil || rangeStart < start || rangeEnd > end || rangeEnd < rangeStart || rangeEnd-rangeStart+1 > maxChunkSize {
			t.Errorf("invalid upstream request %q: %v", rangeHeader, err)
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		bodyLength := rangeEnd - rangeStart + 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rangeStart, rangeEnd, total))
		w.Header().Set("Content-Length", fmt.Sprint(bodyLength))
		w.WriteHeader(http.StatusPartialContent)
		if calls.Add(1) == 1 {
			bodyLength -= 7
		}
		if err := writeChunkPattern(w, rangeStart, bodyLength); err != nil {
			t.Errorf("write upstream body: %v", err)
		}
	}))
	defer upstream.Close()
	handler := &sourceHandler{client: upstream.Client(), source: Resource{URL: upstream.URL, Size: total}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dst := &chunkDigestWriter{hash: sha256.New()}
	if err := handler.copySpan(ctx, dst, start, end, total); err != nil {
		t.Fatal(err)
	}
	expectedDigest := sha256.New()
	if err := writeChunkPattern(expectedDigest, start, length); err != nil {
		t.Fatal(err)
	}
	if dst.bytes != length || !reflect.DeepEqual(dst.hash.Sum(nil), expectedDigest.Sum(nil)) {
		t.Fatalf("forwarded bytes = %d; want %d; digest = %x; want %x", dst.bytes, length, dst.hash.Sum(nil), expectedDigest.Sum(nil))
	}
	wantRanges := []string{
		fmt.Sprintf("bytes=%d-%d", start, start+maxChunkSize-1),
		fmt.Sprintf("bytes=%d-%d", start+maxChunkSize-7, start+maxChunkSize-1),
		fmt.Sprintf("bytes=%d-%d", start+maxChunkSize, start+2*maxChunkSize-1),
		fmt.Sprintf("bytes=%d-%d", start+2*maxChunkSize, end),
	}
	mutex.Lock()
	defer mutex.Unlock()
	if !reflect.DeepEqual(ranges, wantRanges) || calls.Load() != int32(len(wantRanges)) {
		t.Fatalf("ranges = %v, calls = %d; want ranges = %v, calls = %d", ranges, calls.Load(), wantRanges, len(wantRanges))
	}
}
