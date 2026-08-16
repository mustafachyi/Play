package stream

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Reporter func(string, error)

type Resource struct {
	Name   string
	URL    string
	MIME   string
	Size   int64
	Ranged bool
}

type Server struct {
	httpServer *http.Server
	client     *http.Client
	baseURL    string
	token      string
	reporter   Reporter
	mu         sync.RWMutex
	handlers   map[string]http.Handler
	closed     bool
	closeOnce  sync.Once
	closeErr   error
}

func Start(resources []Resource, reporter Reporter) (*Server, error) {
	if len(resources) == 0 {
		return nil, errors.New("no stream resources were provided")
	}
	if err := validateResources(resources); err != nil {
		return nil, err
	}
	return start(resources, reporter)
}

func StartDynamic(reporter Reporter) (*Server, error) {
	return start(nil, reporter)
}

func start(resources []Resource, reporter Reporter) (*Server, error) {
	token, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("create local stream token: %w", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start local stream server: %w", err)
	}
	server := &Server{
		client: newHTTPClient(), baseURL: "http://" + listener.Addr().String(), token: token,
		reporter: reporter, handlers: make(map[string]http.Handler, len(resources)),
	}
	for _, resource := range resources {
		server.handlers[resource.Name] = server.handler(resource)
	}
	server.httpServer = &http.Server{
		Handler: server, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	go func() { _ = server.httpServer.Serve(listener) }()
	return server, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	prefix := "/" + s.token + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, prefix)
	if name == "" || strings.ContainsRune(name, '/') {
		http.NotFound(w, r)
		return
	}
	s.mu.RLock()
	handler := s.handlers[name]
	closed := s.closed
	s.mu.RUnlock()
	if closed || handler == nil {
		http.NotFound(w, r)
		return
	}
	handler.ServeHTTP(w, r)
}

func (s *Server) Add(resources []Resource) error {
	if len(resources) == 0 {
		return nil
	}
	if err := validateResources(resources); err != nil {
		return err
	}
	prepared := make(map[string]http.Handler, len(resources))
	for _, resource := range resources {
		prepared[resource.Name] = s.handler(resource)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("local stream server is closed")
	}
	for name := range prepared {
		if _, exists := s.handlers[name]; exists {
			return fmt.Errorf("duplicate resource name %q", name)
		}
	}
	for name, handler := range prepared {
		s.handlers[name] = handler
	}
	return nil
}

func (s *Server) handler(resource Resource) http.Handler {
	if resource.Ranged {
		return &sourceHandler{client: s.client, source: resource, chunkSize: maxChunkSize, reporter: s.reporter}
	}
	return &assetHandler{client: s.client, source: resource, reporter: s.reporter}
}

func (s *Server) URL(name string) string {
	s.mu.RLock()
	_, ok := s.handlers[name]
	closed := s.closed
	s.mu.RUnlock()
	if !ok || closed {
		return ""
	}
	return s.baseURL + "/" + s.token + "/" + name
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.closeErr = s.httpServer.Close()
		s.client.CloseIdleConnections()
	})
	return s.closeErr
}

func validateResources(resources []Resource) error {
	names := make(map[string]struct{}, len(resources))
	for _, resource := range resources {
		if err := validateResource(resource); err != nil {
			return fmt.Errorf("prepare %q: %w", resource.Name, err)
		}
		if _, exists := names[resource.Name]; exists {
			return fmt.Errorf("duplicate resource name %q", resource.Name)
		}
		names[resource.Name] = struct{}{}
	}
	return nil
}

func validateResource(resource Resource) error {
	if err := validateName(resource.Name); err != nil {
		return err
	}
	if resource.Size < 0 {
		return errors.New("resource size is invalid")
	}
	u, err := url.Parse(resource.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("resource URL is not a valid HTTPS URL")
	}
	return nil
}

func validateName(name string) error {
	if name == "" || len(name) > 200 {
		return errors.New("name length is invalid")
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return errors.New("name contains unsupported characters")
	}
	if name == "." || name == ".." {
		return errors.New("name is invalid")
	}
	return nil
}

func randomToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.MaxIdleConns = 16
	transport.MaxIdleConnsPerHost = 8
	transport.MaxConnsPerHost = 8
	transport.IdleConnTimeout = 30 * time.Second
	transport.ResponseHeaderTimeout = 10 * time.Second
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("upstream redirect changed to a non-HTTPS URL")
			}
			if len(via) >= 5 {
				return errors.New("too many upstream redirects")
			}
			return nil
		},
	}
}
