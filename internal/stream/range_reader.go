package stream

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var errRangeReadStalled = errors.New("upstream range response stalled")

type progressReader struct {
	reader   io.Reader
	cancel   context.CancelFunc
	timeout  time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time
	reading  bool
	timedOut bool
}

func (r *progressReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	r.reading = true
	r.deadline = time.Now().Add(r.timeout)
	if r.timer == nil {
		r.timer = time.AfterFunc(r.timeout, r.cancelStalledRead)
	} else {
		r.timer.Reset(r.timeout)
	}
	r.mu.Unlock()
	n, err := r.reader.Read(p)
	r.mu.Lock()
	r.reading = false
	r.timer.Stop()
	timedOut := r.timedOut
	r.mu.Unlock()
	if timedOut {
		return n, errRangeReadStalled
	}
	return n, err
}

func (r *progressReader) cancelStalledRead() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reading && !time.Now().Before(r.deadline) {
		r.timedOut = true
		r.cancel()
	}
}
