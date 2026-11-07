package youtube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRequestsRecoverAfterTransientFailure(t *testing.T) {
	for _, browse := range []bool{false, true} {
		calls := 0
		var firstBody string
		client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if calls == 1 {
				firstBody = string(body)
				return response(503, `{}`), nil
			}
			if string(body) != firstBody {
				t.Fatal("request body changed on retry")
			}
			return response(200, `{"ok":true}`), nil
		})})
		var body []byte
		var err error
		if browse {
			body, err = client.requestBrowse(context.Background(), "cursor")
		} else {
			body, err = client.requestPlayer(context.Background(), playerProfiles[0], testVideoID, "visitor")
		}
		if err != nil || calls != 2 || !strings.Contains(string(body), "true") {
			t.Fatalf("browse=%v calls=%d body=%s error=%v", browse, calls, body, err)
		}
	}
}

func TestRetryStopsAtLimitAndPreservesError(t *testing.T) {
	calls := 0
	want := &TransportError{Kind: NetworkFailure}
	_, err := retryRequest(context.Background(), func() ([]byte, error) {
		calls++
		return nil, want
	})
	if err != want || calls != 3 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestRetryRejectsPermanentFailures(t *testing.T) {
	for _, want := range []error{
		&TransportError{Kind: HTTPFailure, Status: 403},
		&TransportError{Kind: HTTPFailure, Status: 404},
		&TransportError{Kind: InvalidJSON},
		&TransportError{Kind: ResponseTooLarge},
		&PlayerError{Kind: Unplayable},
		ErrLiveUnsupported,
	} {
		calls := 0
		_, err := retryRequest(context.Background(), func() ([]byte, error) {
			calls++
			return nil, want
		})
		if err != want || calls != 1 {
			t.Fatalf("calls=%d error=%v", calls, err)
		}
	}
}

func TestRetryCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	_, err := retryRequest(ctx, func() ([]byte, error) {
		calls++
		cancel()
		return nil, &TransportError{Kind: Timeout}
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}
