package playlist

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"play/internal/media"
)

type ResolvedItem struct {
	EDL      string
	CoverURL string
}

type Resolver func(context.Context, int, string) (ResolvedItem, error)
type PageLoader func(context.Context) ([]media.PlaylistItem, bool, error)
type Reporter func(int, error)

type routeKind uint8

const (
	entryRoute routeKind = iota
	coverRoute
)

type route struct {
	index int
	kind  routeKind
}

type Server struct {
	httpServer *http.Server
	baseURL    string
	token      string
	playlist   string
	resolver   Resolver
	pageLoader PageLoader
	reporter   Reporter
	mu         sync.RWMutex
	entries    []*entry
	routes     map[string]route
	pageMu     sync.Mutex
	pageMore   bool
	nextPage   int
	pageBodies map[int]string
	closeOnce  sync.Once
	closeErr   error
}

type entry struct {
	mu      sync.Mutex
	item    media.PlaylistItem
	value   ResolvedItem
	lastErr string
}

func Start(items []media.PlaylistItem, resolver Resolver, pageLoader PageLoader, reporter Reporter) (*Server, error) {
	if len(items) == 0 {
		return nil, errors.New("playlist contains no videos")
	}
	if resolver == nil {
		return nil, errors.New("playlist resolver is nil")
	}
	if err := validateItems(items); err != nil {
		return nil, err
	}

	token, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("create local playlist token: %w", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start local playlist server: %w", err)
	}

	server := &Server{
		baseURL:    "http://" + listener.Addr().String(),
		token:      token,
		resolver:   resolver,
		pageLoader: pageLoader,
		reporter:   reporter,
		routes:     make(map[string]route, len(items)*2),
		pageMore:   pageLoader != nil,
		nextPage:   2,
		pageBodies: make(map[int]string),
	}
	server.playlist = server.appendItems(items)
	server.httpServer = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() { _ = server.httpServer.Serve(listener) }()
	return server, nil
}

func (s *Server) appendItems(items []media.PlaylistItem) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n")
	for _, item := range items {
		index := len(s.entries)
		entryPath := fmt.Sprintf("/%s/item/%04d.edl", s.token, index+1)
		coverPath := fmt.Sprintf("/%s/cover/%04d", s.token, index+1)
		s.entries = append(s.entries, &entry{item: item})
		s.routes[entryPath] = route{index: index, kind: entryRoute}
		s.routes[coverPath] = route{index: index, kind: coverRoute}
		playlist.WriteString("#EXTINF:-1,")
		playlist.WriteString(playlistTitle(item))
		playlist.WriteByte('\n')
		playlist.WriteString(s.baseURL)
		playlist.WriteString(entryPath)
		playlist.WriteByte('\n')
	}
	return playlist.String()
}

func validateItems(items []media.PlaylistItem) error {
	for _, item := range items {
		if item.VideoID == "" {
			return errors.New("playlist contains an empty video ID")
		}
	}
	return nil
}

func playlistTitle(item media.PlaylistItem) string {
	title := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\x00' {
			return ' '
		}
		return r
	}, item.Title)
	title = strings.TrimSpace(title)
	if title == "" {
		return item.VideoID
	}
	return title
}

func (s *Server) URL() string {
	return s.baseURL + "/" + s.token + "/playlist.m3u"
}

func (s *Server) PageURL() string {
	if s.pageLoader == nil {
		return ""
	}
	return s.baseURL + "/" + s.token + "/page/"
}

func (s *Server) Prepare(ctx context.Context, index int) error {
	entry := s.entry(index)
	if entry == nil {
		return errors.New("playlist index is outside the playlist")
	}
	_, err := s.resolveEntry(ctx, index, entry, false)
	return err
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.httpServer.Close()
	})
	return s.closeErr
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/"+s.token+"/playlist.m3u" {
		s.servePlaylist(w, r)
		return
	}
	if page, ok := s.pageNumber(r.URL.Path); ok {
		s.servePage(w, r, page)
		return
	}

	s.mu.RLock()
	matched, ok := s.routes[r.URL.Path]
	s.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	entry := s.entry(matched.index)
	if entry == nil {
		http.NotFound(w, r)
		return
	}
	if matched.kind == coverRoute {
		s.serveCover(w, r, matched.index, entry)
		return
	}
	s.serveEntry(w, r, matched.index, entry)
}

func (s *Server) servePlaylist(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.playlist)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, s.playlist)
	}
}

func (s *Server) servePage(w http.ResponseWriter, r *http.Request, page int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	body, err := s.loadPage(r.Context(), page)
	if err != nil {
		http.Error(w, "playlist page unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

func (s *Server) loadPage(ctx context.Context, page int) (string, error) {
	s.pageMu.Lock()
	defer s.pageMu.Unlock()

	if body, ok := s.pageBodies[page]; ok {
		return body, nil
	}
	if page != s.nextPage {
		return "", errors.New("playlist page is out of sequence")
	}
	if !s.pageMore || s.pageLoader == nil {
		body := "#EXTM3U\n"
		s.pageBodies[page] = body
		return body, nil
	}

	items, more, err := s.pageLoader(ctx)
	if err != nil {
		return "", err
	}
	if len(items) == 0 && more {
		return "", errors.New("playlist page loader returned an empty page before completion")
	}
	if err := validateItems(items); err != nil {
		return "", err
	}

	body := "#EXTM3U\n"
	if len(items) > 0 {
		body = s.appendItems(items)
	}
	s.pageMore = more
	s.pageBodies[page] = body
	s.nextPage++
	return body, nil
}

func (s *Server) pageNumber(path string) (int, bool) {
	prefix := "/" + s.token + "/page/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, ".m3u") {
		return 0, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(path, prefix), ".m3u")
	if len(value) != 4 {
		return 0, false
	}
	page, err := strconv.Atoi(value)
	return page, err == nil && page >= 2
}

func (s *Server) serveEntry(w http.ResponseWriter, r *http.Request, index int, entry *entry) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	resolved, err := s.resolveEntry(r.Context(), index, entry, true)
	if err != nil {
		http.Error(w, "playlist item unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(resolved.EDL)))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, resolved.EDL)
}

func (s *Server) serveCover(w http.ResponseWriter, r *http.Request, index int, entry *entry) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	resolved, err := s.resolveEntry(r.Context(), index, entry, true)
	if err != nil {
		http.Error(w, "playlist item unavailable", http.StatusBadGateway)
		return
	}
	if resolved.CoverURL == "" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, resolved.CoverURL, http.StatusTemporaryRedirect)
}

func (s *Server) entry(index int) *entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if index < 0 || index >= len(s.entries) {
		return nil
	}
	return s.entries[index]
}

func (s *Server) resolveEntry(ctx context.Context, index int, entry *entry, report bool) (ResolvedItem, error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.value.EDL != "" {
		return entry.value, nil
	}
	resolved, err := s.resolver(ctx, index, entry.item.VideoID)
	if err == nil && resolved.EDL == "" {
		err = errors.New("playlist resolver returned an empty item")
	}
	if err == nil && resolved.CoverURL != "" && !localHTTPURL(resolved.CoverURL) {
		err = errors.New("playlist resolver returned a non-local cover URL")
	}
	if err != nil {
		if report && ctx.Err() == nil && s.reporter != nil && entry.lastErr != err.Error() {
			entry.lastErr = err.Error()
			s.reporter(index, err)
		}
		return ResolvedItem{}, err
	}
	entry.value = resolved
	entry.lastErr = ""
	return resolved, nil
}

func localHTTPURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return false
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func randomToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
