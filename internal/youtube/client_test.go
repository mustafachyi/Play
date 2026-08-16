package youtube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func testClient(httpClient *http.Client) *Client {
	return &Client{http: httpClient}
}

func TestReadResponseRejectsInvalidJSON(t *testing.T) {
	client := testClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, "not-json"), nil
	})})
	_, err := client.requestPlayer(context.Background(), playerProfiles[0], "dQw4w9WgXcQ", "")
	var upstream *TransportError
	if !errors.As(err, &upstream) || upstream.Kind != InvalidJSON {
		t.Fatalf("error = %#v", err)
	}
}

func TestRequestPlayerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})})
	_, err := client.requestPlayer(ctx, playerProfiles[0], "dQw4w9WgXcQ", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %#v", err)
	}
}

func TestTransportErrorsAreClassified(t *testing.T) {
	client := testClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusServiceUnavailable, `{}`), nil
	})})
	_, err := client.requestPlayer(context.Background(), playerProfiles[0], testVideoID, "")
	var upstream *TransportError
	if !errors.As(err, &upstream) || upstream.Kind != HTTPFailure || upstream.Status != http.StatusServiceUnavailable || upstream.Error() != "upstream HTTP error: HTTP 503" {
		t.Fatalf("error = %#v", err)
	}

	client = testClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})})
	_, err = client.requestPlayer(context.Background(), playerProfiles[0], testVideoID, "")
	if !errors.As(err, &upstream) || upstream.Kind != NetworkFailure {
		t.Fatalf("error = %#v", err)
	}
}

func TestReadResponseRejectsDeclaredOversizeBody(t *testing.T) {
	resp := response(http.StatusOK, `{}`)
	resp.ContentLength = maxResponseSize + 1
	_, err := readResponse(context.Background(), context.Background(), resp, nil)
	var upstream *TransportError
	if !errors.As(err, &upstream) || upstream.Kind != ResponseTooLarge {
		t.Fatalf("error = %#v", err)
	}
}
