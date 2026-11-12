package youtube

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"play/internal/httpretry"
)

func TestFetchPlayerResponsesPreservesRetryAfterLimit(t *testing.T) {
	var callsMutex sync.Mutex
	calls := make(map[string]int)
	client := testClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		key := req.Header.Get("X-YouTube-Client-Name") + "|" + req.Header.Get("X-YouTube-Client-Version")
		callsMutex.Lock()
		calls[key]++
		callsMutex.Unlock()
		resp := response(http.StatusServiceUnavailable, `{}`)
		resp.Header.Set("Retry-After", "31")
		return resp, nil
	})})
	_, err := client.fetchPlayerResponses(context.Background(), testVideoID)
	var upstream *TransportError
	if !errors.Is(err, httpretry.ErrDelayTooLong) || !errors.As(err, &upstream) || upstream.Kind != HTTPFailure || upstream.Status != http.StatusServiceUnavailable {
		t.Fatalf("error = %v", err)
	}
	callsMutex.Lock()
	defer callsMutex.Unlock()
	for _, profile := range playerProfiles {
		key := profile.numericID + "|" + profile.context.ClientVersion
		if calls[key] != 1 {
			t.Fatalf("profile %s made %d requests; want 1", key, calls[key])
		}
	}
}
