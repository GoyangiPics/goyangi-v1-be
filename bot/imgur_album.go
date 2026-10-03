package bot

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"goyangi-v1-be/hooks"
)

// Imgur album and gallery links wrap media rather than being media, so they
// have to be resolved into the items they hold before anything can be
// downloaded.
//
// This used to read the album page's OpenGraph tags. As of 2026-09 imgur renders
// its pages client-side and serves no og:video/og:image to any user agent — a
// crawler UA gets the imgur logo — so that path resolves nothing, silently. The
// post API still answers, and unlike the page it lists EVERY item in an album
// rather than only the cover, which the OpenGraph path never could.
//
// The API wants a client id (IMGUR_CLIENT_ID, see config.go). Without one the
// OpenGraph path is still tried, so a deploy that hasn't set it behaves exactly
// as before rather than failing in a new way.

// imgurPostAPI is imgur's post endpoint. `albums` answers /a/ links, `posts`
// answers /gallery/ links; both return the same shape.
const imgurPostAPI = "https://api.imgur.com/post/v1/"

// fetchImgurAlbumItems resolves one album or gallery permalink into its media.
// Nil means nothing ingestible: the album is gone, empty, or could not be
// resolved (each already logged).
func fetchImgurAlbumItems(pageURL string) []MediaItem {
	if clientID := imgurClientID(); clientID != "" {
		items, resolved := fetchImgurAlbumViaAPI(pageURL, clientID)
		if resolved {
			return items
		}
		// A transport-level failure, not a verdict on the album: fall through
		// to the page in case it has something to say.
	}
	if item, ok := fetchImgurAlbumItem(pageURL); ok {
		return []MediaItem{item}
	}
	return nil
}

// fetchImgurAlbumViaAPI asks the post API for the album's media. `resolved` is
// false only when the API could not be reached or answered unexpectedly; an
// album that is gone (404/410) is resolved to nothing.
func fetchImgurAlbumViaAPI(pageURL, clientID string) (items []MediaItem, resolved bool) {
	m := imgurAlbumRegexp.FindStringSubmatch(pageURL)
	if m == nil {
		return nil, false
	}
	kind, id := "albums", imgurID(m[2])
	if m[1] == "gallery" {
		kind = "posts"
	}
	if id == "" {
		return nil, true
	}

	q := url.Values{"client_id": {clientID}, "include": {"media"}}
	req, err := http.NewRequest(http.MethodGet, imgurPostAPI+kind+"/"+id+"?"+q.Encode(), nil)
	if err != nil {
		return nil, false
	}
	resp, err := imgurAlbumClient.Do(req)
	if err != nil {
		hooks.LogBotWarning("imgur album: API fetch failed: "+err.Error(), map[string]any{"url": pageURL})
		return nil, false
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		// The album itself is gone. Nothing to ingest, and nothing the page
		// could add — resolved, empty.
		slog.Info("imgur album no longer exists", "url", pageURL)
		return nil, true
	default:
		hooks.LogBotWarning(fmt.Sprintf("imgur album: API status %s", resp.Status), map[string]any{"url": pageURL})
		return nil, false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		hooks.LogBotWarning("imgur album: API read failed: "+err.Error(), map[string]any{"url": pageURL})
		return nil, false
	}
	items, err = imgurItemsFromAPI(body)
	if err != nil {
		hooks.LogBotWarning("imgur album: API response unreadable: "+err.Error(), map[string]any{"url": pageURL})
		return nil, false
	}
	if len(items) == 0 {
		slog.Info("imgur album is empty", "url", pageURL)
	}
	return items, true
}

// imgurItemsFromAPI turns a post API response into media items, one per entry.
// Each url goes through the same normalisation as a plain imgur link
// (imgurMatchToItem), so filetype and the /revive mirror come out identical
// whether a clip was posted directly or inside an album. Split from the fetch to
// stay testable.
func imgurItemsFromAPI(body []byte) ([]MediaItem, error) {
	var post struct {
		Media []struct {
			URL string `json:"url"`
		} `json:"media"`
	}
	if err := json.Unmarshal(body, &post); err != nil {
		return nil, err
	}
	items := make([]MediaItem, 0, len(post.Media))
	for _, m := range post.Media {
		im := imgurRegexp.FindStringSubmatch(m.URL)
		if im == nil {
			continue
		}
		if item, ok := imgurMatchToItem(im); ok {
			items = append(items, item)
		}
	}
	return items, nil
}
