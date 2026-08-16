package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	requestTimeout  = 10 * time.Second
	maxResponseSize = 8 << 20
)

type TransportErrorKind string

const (
	Timeout          TransportErrorKind = "upstream timeout"
	HTTPFailure      TransportErrorKind = "upstream HTTP error"
	InvalidJSON      TransportErrorKind = "invalid upstream JSON"
	NetworkFailure   TransportErrorKind = "upstream network error"
	ResponseTooLarge TransportErrorKind = "upstream response too large"
)

type TransportError struct {
	Kind   TransportErrorKind
	Status int
}

func (e *TransportError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s: HTTP %d", e.Kind, e.Status)
	}
	return string(e.Kind)
}

type Client struct {
	http *http.Client
}

func NewClient() *Client {
	return &Client{http: newHTTPClient()}
}

func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

func readResponse(ctx, requestCtx context.Context, resp *http.Response, requestErr error) ([]byte, error) {
	if requestErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if requestCtx.Err() == context.DeadlineExceeded {
			return nil, &TransportError{Kind: Timeout}
		}
		return nil, &TransportError{Kind: NetworkFailure}
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		return nil, &TransportError{Kind: HTTPFailure, Status: resp.StatusCode}
	}
	if resp.ContentLength > maxResponseSize {
		return nil, &TransportError{Kind: ResponseTooLarge}
	}

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if requestCtx.Err() == context.DeadlineExceeded {
			return nil, &TransportError{Kind: Timeout}
		}
		return nil, &TransportError{Kind: NetworkFailure}
	}
	if len(responseBody) > maxResponseSize {
		return nil, &TransportError{Kind: ResponseTooLarge}
	}
	if !json.Valid(responseBody) {
		return nil, &TransportError{Kind: InvalidJSON}
	}
	return responseBody, nil
}

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 16
	transport.MaxIdleConnsPerHost = 8
	transport.MaxConnsPerHost = 8
	transport.IdleConnTimeout = 30 * time.Second
	transport.ResponseHeaderTimeout = requestTimeout
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func transportError(err error) *TransportError {
	var upstream *TransportError
	if errors.As(err, &upstream) {
		return upstream
	}
	return nil
}
