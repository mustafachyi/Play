package youtube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"play/internal/media"
)

const (
	browseEndpoint                       = "https://www.youtube.com/youtubei/v1/browse?prettyPrint=false"
	playlistContinuationPropertiesBase64 = "CADCBgIIAA%3D%3D"
	maxPlaylistItems                     = 5000
	maxPlaylistPages                     = 1000
)

type browseRequest struct {
	Context struct {
		Client clientContext `json:"client"`
	} `json:"context"`
	Continuation string `json:"continuation"`
}

type Playlist struct {
	request      func(context.Context, string) ([]byte, error)
	continuation string
	seen         map[string]struct{}
	pages        int
	items        int
	done         bool
}

func (c *Client) Playlist(playlistID string) (*Playlist, error) {
	return newPlaylist(playlistID, c.requestBrowse)
}

func newPlaylist(playlistID string, request func(context.Context, string) ([]byte, error)) (*Playlist, error) {
	if playlistID == "" {
		return nil, errors.New("playlist ID is empty")
	}
	if request == nil {
		return nil, errors.New("playlist request function is nil")
	}
	return &Playlist{
		request:      request,
		continuation: initialPlaylistContinuation(playlistID),
		seen:         make(map[string]struct{}),
	}, nil
}

func (p *Playlist) Next(ctx context.Context) ([]media.PlaylistItem, bool, error) {
	if p.done {
		return nil, false, nil
	}

	for {
		if p.pages >= maxPlaylistPages {
			return nil, false, errors.New("playlist returned too many continuation pages")
		}
		continuation := p.continuation
		if continuation == "" {
			p.done = true
			if p.items == 0 {
				return nil, false, errors.New("playlist contains no videos")
			}
			return nil, false, nil
		}
		if _, exists := p.seen[continuation]; exists {
			return nil, false, errors.New("playlist returned a repeated continuation")
		}

		body, err := p.request(ctx, continuation)
		if err != nil {
			return nil, false, err
		}
		values, err := playlistContinuationItems(body)
		if err != nil {
			return nil, false, err
		}

		items := make([]media.PlaylistItem, 0, len(values))
		for _, value := range values {
			item, ok := playlistItem(value)
			if ok {
				items = append(items, item)
			}
		}
		if p.items+len(items) > maxPlaylistItems {
			return nil, false, errors.New("playlist exceeds the 5000-item safety limit")
		}

		next := playlistContinuationToken(values)
		if next != "" {
			if next == continuation {
				return nil, false, errors.New("playlist returned a repeated continuation")
			}
			if _, exists := p.seen[next]; exists {
				return nil, false, errors.New("playlist returned a repeated continuation")
			}
		}

		p.seen[continuation] = struct{}{}
		p.pages++
		p.items += len(items)
		p.continuation = next
		p.done = next == ""

		if len(items) > 0 {
			return items, !p.done, nil
		}
		if p.done {
			if p.items == 0 {
				return nil, false, errors.New("playlist contains no videos")
			}
			return nil, false, nil
		}
	}
}

func (c *Client) requestBrowse(ctx context.Context, continuation string) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	requestBody := browseRequest{Continuation: continuation}
	requestBody.Context.Client = browseProfile.context
	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, browseEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://www.youtube.com")
	req.Header.Set("Referer", "https://www.youtube.com/")
	req.Header.Set("X-YouTube-Client-Name", browseProfile.numericID)
	req.Header.Set("X-YouTube-Client-Version", browseProfile.context.ClientVersion)
	resp, err := c.http.Do(req)
	return readResponse(ctx, requestCtx, resp, err)
}

func initialPlaylistContinuation(playlistID string) string {
	parameters := make([]byte, 0, len(playlistID)*2+32)
	parameters = appendProtoString(parameters, 2, "VL"+playlistID)
	parameters = appendProtoString(parameters, 3, playlistContinuationPropertiesBase64)
	parameters = appendProtoString(parameters, 35, playlistID)

	message := make([]byte, 0, len(parameters)+8)
	message = appendProtoBytes(message, 80226972, parameters)
	return url.QueryEscape(base64.URLEncoding.EncodeToString(message))
}

func appendProtoString(dst []byte, field int, value string) []byte {
	return appendProtoBytes(dst, field, []byte(value))
}

func appendProtoBytes(dst []byte, field int, value []byte) []byte {
	dst = appendProtoVarint(dst, uint64(field)<<3|2)
	dst = appendProtoVarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendProtoVarint(dst []byte, value uint64) []byte {
	for value >= 0x80 {
		dst = append(dst, byte(value)|0x80)
		value >>= 7
	}
	return append(dst, byte(value))
}

type playlistBrowseResponse struct {
	OnResponseReceivedActions   []playlistContinuationAction `json:"onResponseReceivedActions"`
	OnResponseReceivedEndpoints []playlistContinuationAction `json:"onResponseReceivedEndpoints"`
	ContinuationContents        struct {
		PlaylistVideoListContinuation playlistContinuationCommand `json:"playlistVideoListContinuation"`
	} `json:"continuationContents"`
}

type playlistContinuationAction struct {
	ReloadContinuationItemsCommand playlistContinuationCommand `json:"reloadContinuationItemsCommand"`
	AppendContinuationItemsAction  playlistContinuationCommand `json:"appendContinuationItemsAction"`
}

type playlistContinuationCommand struct {
	ContinuationItems []json.RawMessage `json:"continuationItems"`
	Contents          []json.RawMessage `json:"contents"`
}

func playlistContinuationItems(body []byte) ([]json.RawMessage, error) {
	var response playlistBrowseResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, errors.New("invalid playlist response")
	}

	for _, actions := range [][]playlistContinuationAction{
		response.OnResponseReceivedActions,
		response.OnResponseReceivedEndpoints,
	} {
		for _, action := range actions {
			if len(action.ReloadContinuationItemsCommand.ContinuationItems) > 0 {
				return action.ReloadContinuationItemsCommand.ContinuationItems, nil
			}
			if len(action.AppendContinuationItemsAction.ContinuationItems) > 0 {
				return action.AppendContinuationItemsAction.ContinuationItems, nil
			}
		}
	}

	fallback := response.ContinuationContents.PlaylistVideoListContinuation
	if len(fallback.ContinuationItems) > 0 {
		return fallback.ContinuationItems, nil
	}
	if len(fallback.Contents) > 0 {
		return fallback.Contents, nil
	}
	return nil, nil
}

type playlistContinuationItem struct {
	PlaylistVideoRenderer *struct {
		VideoID string      `json:"videoId"`
		Title   displayText `json:"title"`
	} `json:"playlistVideoRenderer"`
	RichItemRenderer *struct {
		Content struct {
			ReelItemRenderer *struct {
				VideoID  string      `json:"videoId"`
				Headline displayText `json:"headline"`
				Title    displayText `json:"title"`
			} `json:"reelItemRenderer"`
		} `json:"content"`
	} `json:"richItemRenderer"`
	LockupViewModel *struct {
		ContentID   string `json:"contentId"`
		ContentType string `json:"contentType"`
		Metadata    struct {
			LockupMetadataViewModel struct {
				Title struct {
					Content string `json:"content"`
				} `json:"title"`
			} `json:"lockupMetadataViewModel"`
		} `json:"metadata"`
	} `json:"lockupViewModel"`
	ContinuationItemRenderer *struct {
		ContinuationEndpoint struct {
			ContinuationCommand struct {
				Token string `json:"token"`
			} `json:"continuationCommand"`
			CommandExecutorCommand struct {
				Commands []struct {
					ContinuationCommand struct {
						Token string `json:"token"`
					} `json:"continuationCommand"`
				} `json:"commands"`
			} `json:"commandExecutorCommand"`
		} `json:"continuationEndpoint"`
	} `json:"continuationItemRenderer"`
	ContinuationItemViewModel *struct {
		ContinuationCommand struct {
			InnertubeCommand struct {
				ContinuationCommand struct {
					Token string `json:"token"`
				} `json:"continuationCommand"`
			} `json:"innertubeCommand"`
		} `json:"continuationCommand"`
	} `json:"continuationItemViewModel"`
}

func playlistItem(raw json.RawMessage) (media.PlaylistItem, bool) {
	var item playlistContinuationItem
	if json.Unmarshal(raw, &item) != nil {
		return media.PlaylistItem{}, false
	}
	if renderer := item.PlaylistVideoRenderer; renderer != nil {
		videoID := validVideoID(renderer.VideoID)
		return media.PlaylistItem{VideoID: videoID, Title: cleanTitle(renderer.Title.String())}, videoID != ""
	}
	if renderer := item.RichItemRenderer; renderer != nil && renderer.Content.ReelItemRenderer != nil {
		reel := renderer.Content.ReelItemRenderer
		title := reel.Headline.String()
		if strings.TrimSpace(title) == "" {
			title = reel.Title.String()
		}
		videoID := validVideoID(reel.VideoID)
		return media.PlaylistItem{VideoID: videoID, Title: cleanTitle(title)}, videoID != ""
	}
	if lockup := item.LockupViewModel; lockup != nil && lockup.ContentType == "LOCKUP_CONTENT_TYPE_VIDEO" {
		videoID := validVideoID(lockup.ContentID)
		return media.PlaylistItem{VideoID: videoID, Title: cleanTitle(lockup.Metadata.LockupMetadataViewModel.Title.Content)}, videoID != ""
	}
	return media.PlaylistItem{}, false
}

func cleanTitle(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\x00' {
			return ' '
		}
		return r
	}, value)
	return strings.TrimSpace(value)
}

func playlistContinuationToken(items []json.RawMessage) string {
	for i := len(items) - 1; i >= 0; i-- {
		var item playlistContinuationItem
		if json.Unmarshal(items[i], &item) != nil {
			continue
		}
		if renderer := item.ContinuationItemRenderer; renderer != nil {
			for _, command := range renderer.ContinuationEndpoint.CommandExecutorCommand.Commands {
				if command.ContinuationCommand.Token != "" {
					return command.ContinuationCommand.Token
				}
			}
			if token := renderer.ContinuationEndpoint.ContinuationCommand.Token; token != "" {
				return token
			}
		}
		if viewModel := item.ContinuationItemViewModel; viewModel != nil {
			if token := viewModel.ContinuationCommand.InnertubeCommand.ContinuationCommand.Token; token != "" {
				return token
			}
		}
	}
	return ""
}

func validVideoID(value string) string {
	if len(value) != 11 {
		return ""
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return ""
	}
	return value
}
