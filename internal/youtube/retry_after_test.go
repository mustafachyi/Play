package youtube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"play/internal/httpretry"
)

func TestRequestsRespectUpstreamRetryAfter(t *testing.T) {
	for _, browse := range []bool{false, true} {
		kind := "player"
		if browse {
			kind = "playlist"
		}
		for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			t.Run(kind+"/"+http.StatusText(status), func(t *testing.T) {
				var calls atomic.Int32
				var firstRequest atomic.Int64
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					firstRequest.CompareAndSwap(0, time.Now().UnixNano())
					if time.Now().Before(time.Unix(0, firstRequest.Load()).Add(time.Second)) {
						w.Header().Set("Retry-After", "1")
						w.WriteHeader(status)
						_, _ = w.Write([]byte(`{}`))
						return
					}
					_, _ = w.Write([]byte(`{"ok":true}`))
				}))
				defer server.Close()
				upstream := server.Client()
				defer upstream.CloseIdleConnections()
				endpoint, err := url.Parse(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					forwarded := req.Clone(req.Context())
					forwarded.URL.Scheme, forwarded.URL.Host = endpoint.Scheme, endpoint.Host
					return upstream.Transport.RoundTrip(forwarded)
				})})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var body []byte
				if browse {
					body, err = client.requestBrowse(ctx, "cursor")
				} else {
					body, err = client.requestPlayer(ctx, playerProfiles[0], testVideoID, "")
				}
				if err != nil || calls.Load() != 2 || string(body) != `{"ok":true}` {
					t.Fatalf("calls = %d, body = %s, error = %v", calls.Load(), body, err)
				}
			})
		}
	}
}

func TestRetryAfterStopsAtWaitBudgetAndPreservesHTTPFailure(t *testing.T) {
	for _, value := range []string{"31", "99999999999999999999999999999999999999999", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)} {
		calls := 0
		client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			resp := response(http.StatusServiceUnavailable, `{}`)
			resp.Header.Set("Retry-After", value)
			return resp, nil
		})})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := client.requestPlayer(ctx, playerProfiles[0], testVideoID, "")
		cancel()
		var upstream *TransportError
		if calls != 1 || !errors.Is(err, httpretry.ErrDelayTooLong) || !errors.As(err, &upstream) || upstream.Kind != HTTPFailure || upstream.Status != http.StatusServiceUnavailable || upstream.Error() != "upstream HTTP error: HTTP 503" {
			t.Fatalf("hint = %q, calls = %d, error = %v", value, calls, err)
		}
	}
}

func TestRetryAfterCancellationStopsBeforeAnotherRequest(t *testing.T) {
	calls := 0
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		resp := response(http.StatusTooManyRequests, `{}`)
		resp.Header.Set("Retry-After", "30")
		return resp, nil
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := client.requestBrowse(ctx, "cursor")
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("calls = %d, error = %v", calls, err)
	}
}

func TestRetryAfterDoesNotResetAttemptBudget(t *testing.T) {
	calls := 0
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		resp := response(http.StatusTooManyRequests, `{}`)
		resp.Header.Set("Retry-After", "0")
		return resp, nil
	})})
	_, err := client.requestBrowse(context.Background(), "cursor")
	var upstream *TransportError
	if calls != 3 || !errors.As(err, &upstream) || upstream.Status != http.StatusTooManyRequests {
		t.Fatalf("calls = %d, error = %v", calls, err)
	}
}

func TestHTTPFailureDoesNotWaitForRejectedResponseBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	upstream := server.Client()
	defer upstream.CloseIdleConnections()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		forwarded := req.Clone(req.Context())
		forwarded.URL.Scheme, forwarded.URL.Host = endpoint.Scheme, endpoint.Host
		return upstream.Transport.RoundTrip(forwarded)
	})})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	before := time.Now()
	_, err = client.requestPlayerOnce(ctx, playerProfiles[0], testVideoID, "")
	var failure *TransportError
	if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable || failure.RetryAfter != "1" || failure.ReceivedAt.Before(before) || failure.ReceivedAt.After(time.Now()) {
		t.Fatalf("error = %v", err)
	}
}
