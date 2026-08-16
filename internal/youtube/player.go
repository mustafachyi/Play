package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"play/internal/media"
)

const (
	playerEndpoint       = "https://www.youtube.com/youtubei/v1/player?prettyPrint=false"
	maxVisitorDataLength = 4096
)

var ErrLiveUnsupported = errors.New("live streams are not supported")

type PlayerErrorKind string

const (
	Unplayable PlayerErrorKind = "video unplayable"
	Protocol   PlayerErrorKind = "invalid player response"
)

type PlayerError struct {
	Kind    PlayerErrorKind
	Status  string
	Message string
}

func (e *PlayerError) Error() string {
	if e.Status != "" && e.Message != "" {
		return fmt.Sprintf("%s (%s): %s", e.Kind, e.Status, e.Message)
	}
	if e.Status != "" {
		return fmt.Sprintf("%s (%s)", e.Kind, e.Status)
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Kind, e.Message)
	}
	return string(e.Kind)
}

func (c *Client) Video(ctx context.Context, videoID string) (media.Item, error) {
	responses, err := c.fetchPlayerResponses(ctx, videoID)
	if err != nil {
		return media.Item{}, err
	}
	return parsePlayerResponses(responses, videoID)
}

func (c *Client) fetchPlayerResponses(ctx context.Context, videoID string) ([][]byte, error) {
	type result struct {
		body []byte
		err  error
	}

	results := make([]result, len(playerProfiles))
	done := make(chan struct{}, len(playerProfiles))
	for i := range playerProfiles {
		go func(i int) {
			results[i].body, results[i].err = c.fetchPlayer(ctx, playerProfiles[i], videoID)
			done <- struct{}{}
		}(i)
	}
	for range playerProfiles {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	responses := make([][]byte, 0, len(results))
	for _, result := range results {
		if result.err == nil {
			responses = append(responses, result.body)
		}
	}
	if len(responses) > 0 {
		return responses, nil
	}
	for _, result := range results {
		if upstream := transportError(result.err); upstream != nil {
			return nil, upstream
		}
	}
	return nil, &TransportError{Kind: NetworkFailure}
}

func (c *Client) fetchPlayer(ctx context.Context, profile clientProfile, videoID string) ([]byte, error) {
	initial, err := c.requestPlayer(ctx, profile, videoID, "")
	if err != nil {
		return nil, err
	}
	if !requiresVisitorRetry(initial) {
		return initial, nil
	}

	visitorData := extractVisitorData(initial)
	if visitorData == "" {
		return initial, nil
	}

	retried, err := c.requestPlayer(ctx, profile, videoID, visitorData)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return initial, nil
	}
	return retried, nil
}

type playerRequest struct {
	Context         requestContext  `json:"context"`
	VideoID         string          `json:"videoId"`
	PlaybackContext playbackContext `json:"playbackContext"`
	ContentCheckOK  bool            `json:"contentCheckOk"`
	RacyCheckOK     bool            `json:"racyCheckOk"`
}

type requestContext struct {
	Client clientContext `json:"client"`
}

type playbackContext struct {
	ContentPlaybackContext contentPlaybackContext `json:"contentPlaybackContext"`
}

type contentPlaybackContext struct {
	HTML5Preference string `json:"html5Preference"`
}

func (c *Client) requestPlayer(ctx context.Context, profile clientProfile, videoID, visitorData string) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	profileContext := profile.context
	profileContext.VisitorData = visitorData
	body, err := json.Marshal(playerRequest{
		Context: requestContext{Client: profileContext},
		VideoID: videoID,
		PlaybackContext: playbackContext{
			ContentPlaybackContext: contentPlaybackContext{HTML5Preference: "HTML5_PREF_WANTS"},
		},
		ContentCheckOK: true,
		RacyCheckOK:    true,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, playerEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setPlayerHeaders(req, profile, visitorData)
	resp, err := c.http.Do(req)
	return readResponse(ctx, requestCtx, resp, err)
}

func setPlayerHeaders(req *http.Request, profile clientProfile, visitorData string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://www.youtube.com")
	req.Header.Set("User-Agent", profile.context.UserAgent)
	req.Header.Set("X-YouTube-Client-Name", profile.numericID)
	req.Header.Set("X-YouTube-Client-Version", profile.context.ClientVersion)
	if visitorData != "" {
		req.Header.Set("X-Goog-Visitor-Id", visitorData)
	}
}

func requiresVisitorRetry(value []byte) bool {
	var envelope struct {
		PlayabilityStatus struct {
			Status string `json:"status"`
		} `json:"playabilityStatus"`
		StreamingData json.RawMessage `json:"streamingData"`
	}
	if json.Unmarshal(value, &envelope) != nil {
		return false
	}
	return envelope.PlayabilityStatus.Status == "LOGIN_REQUIRED" && !jsonObject(envelope.StreamingData)
}

func extractVisitorData(value []byte) string {
	var envelope struct {
		ResponseContext struct {
			VisitorData string `json:"visitorData"`
		} `json:"responseContext"`
	}
	if json.Unmarshal(value, &envelope) != nil {
		return ""
	}
	visitorData := envelope.ResponseContext.VisitorData
	if visitorData == "" || len(visitorData) > maxVisitorDataLength || strings.ContainsAny(visitorData, "\r\n") {
		return ""
	}
	return visitorData
}

func jsonObject(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}
