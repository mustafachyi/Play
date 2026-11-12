package stream

import (
	"container/list"
	"context"
	"errors"
	"image"
	"os"
	"sync"
)

type previewPage struct {
	key      string
	metadata previewMetadata
	size     int64
}

type previewCache struct {
	mu        sync.Mutex
	jobs      chan struct{}
	done      chan struct{}
	entries   map[string]*list.Element
	order     list.List
	directory string
	bytes     int64
	limit     int64
	closed    bool
	imageKey  string
	image     image.Image
}

func newPreviewCache(limit int64) *previewCache {
	return &previewCache{jobs: make(chan struct{}, 1), done: make(chan struct{}), entries: make(map[string]*list.Element), limit: limit}
}

func (c *previewCache) decoded(key string) (image.Image, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("preview cache is closed")
	}
	if c.imageKey == key {
		return c.image, nil
	}
	return nil, nil
}

func (c *previewCache) retainDecoded(ctx context.Context, key string, decoded image.Image) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("preview cache is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.imageKey, c.image = key, decoded
	return nil
}

func (c *previewCache) lookup(key string) (previewMetadata, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return previewMetadata{}, false, errors.New("preview cache is closed")
	}
	if element := c.entries[key]; element != nil {
		entry := element.Value.(previewPage)
		if _, err := os.Stat(entry.metadata.Path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return previewMetadata{}, false, err
			}
			delete(c.entries, key)
			c.order.Remove(element)
			c.bytes -= entry.size
			return previewMetadata{}, false, nil
		}
		c.order.MoveToFront(element)
		return entry.metadata, true, nil
	}
	return previewMetadata{}, false, nil
}

func (c *previewCache) get(ctx context.Context, key string, load func() ([]byte, previewMetadata, error)) (previewMetadata, error) {
	if err := ctx.Err(); err != nil {
		return previewMetadata{}, err
	}
	if metadata, found, err := c.lookup(key); found || err != nil {
		return metadata, err
	}
	select {
	case <-ctx.Done():
		return previewMetadata{}, ctx.Err()
	case <-c.done:
		return previewMetadata{}, errors.New("preview cache is closed")
	case c.jobs <- struct{}{}:
	}
	defer func() { <-c.jobs }()
	if err := ctx.Err(); err != nil {
		return previewMetadata{}, err
	}
	if metadata, found, err := c.lookup(key); found || err != nil {
		return metadata, err
	}
	data, metadata, err := load()
	if err != nil {
		return previewMetadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return previewMetadata{}, err
	}
	if len(data) == 0 || int64(len(data)) > c.limit {
		return previewMetadata{}, errors.New("preview page exceeds the cache limit")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return previewMetadata{}, errors.New("preview cache is closed")
	}
	for c.bytes+int64(len(data)) > c.limit || len(c.entries) >= previewEntryLimit {
		if err := c.evict(); err != nil {
			return previewMetadata{}, err
		}
	}
	if c.directory == "" {
		c.directory, err = os.MkdirTemp("", "play-preview-")
		if err != nil {
			return previewMetadata{}, err
		}
	}
	file, err := os.CreateTemp(c.directory, "page-*.bgra")
	if err != nil {
		return previewMetadata{}, err
	}
	path := file.Name()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr, ctx.Err()); err != nil {
		return previewMetadata{}, errors.Join(err, os.Remove(path))
	}
	metadata.Path = path
	entry := previewPage{key: key, metadata: metadata, size: int64(len(data))}
	c.entries[key] = c.order.PushFront(entry)
	c.bytes += entry.size
	return metadata, nil
}

func (c *previewCache) evict() error {
	for element := c.order.Back(); element != nil; element = element.Prev() {
		entry := element.Value.(previewPage)
		if err := os.Remove(entry.metadata.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		delete(c.entries, entry.key)
		c.order.Remove(element)
		c.bytes -= entry.size
		return nil
	}
	return errors.New("preview cache files are in use")
}

func (c *previewCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	c.imageKey, c.image = "", nil
	if c.directory == "" {
		return nil
	}
	if err := os.RemoveAll(c.directory); err != nil {
		return err
	}
	c.directory, c.bytes = "", 0
	c.entries = make(map[string]*list.Element)
	c.order.Init()
	return nil
}
