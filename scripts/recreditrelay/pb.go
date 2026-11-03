package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PocketBase over HTTP. contents, contents_sets and uploaders are publicly
// listable, so the dry run reads them with no token at all; -commit writes as a
// superuser, because contents.updateRule lets an owner change nothing about
// who a post is credited to.

type pbClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newPBClient(baseURL string) *pbClient {
	return &pbClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *pbClient) authenticate(email, password string) error {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(http.MethodPost, "/api/collections/_superusers/auth-with-password",
		map[string]string{"identity": email, "password": password}, &out)
	if err != nil {
		return err
	}
	if out.Token == "" {
		return fmt.Errorf("no token in response")
	}
	c.token = out.Token
	return nil
}

// do sends one JSON request and decodes the JSON response into out (if non-nil).
// A non-2xx status is an error carrying PocketBase's message body, which is
// where the field-level validation detail lives.
func (c *pbClient) do(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// listAll pages through a whole collection, or a filtered slice of it.
//
// Collected up front rather than paged while writing: each write takes a post
// out of the relay's filter, so paging through a shrinking result would skip
// posts.
func listAll[T any](c *pbClient, collection, filter, sort, fields string) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("perPage", "500")
		q.Set("sort", sort)
		if filter != "" {
			q.Set("filter", filter)
		}
		if fields != "" {
			q.Set("fields", fields)
		}
		var out struct {
			TotalPages int `json:"totalPages"`
			Items      []T `json:"items"`
		}
		if err := c.do(http.MethodGet, "/api/collections/"+collection+"/records?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Items...)
		if page >= out.TotalPages || len(out.Items) == 0 {
			return all, nil
		}
	}
}

func (c *pbClient) getUploader(id string) (uploader, error) {
	var out uploader
	err := c.do(http.MethodGet, "/api/collections/uploaders/records/"+url.PathEscape(id)+"?fields="+uploaderFields, nil, &out)
	return out, err
}

const uploaderFields = "id,name,aliases,blockIngest,skipDiscordImport"

// uploadersNewestFirst is the whole directory in findUploaderByNames' order.
func (c *pbClient) uploadersNewestFirst() ([]uploader, error) {
	return listAll[uploader](c, "uploaders", "", "-created", uploaderFields)
}

// postsCreditedTo is every post on the uploader, oldest first.
func (c *pbClient) postsCreditedTo(uploaderID string) ([]post, error) {
	return listAll[post](c, "contents", "uploader = "+strconv.Quote(uploaderID), "created", "id,set,discord")
}

// setsListing is the ids of the sets whose (derived) uploaders include uploaderID.
//
// With `:each`, as hooks.setsListingUploader explains: a bare `uploader ?= "…"`
// on this multi relation matches no set at all.
func (c *pbClient) setsListing(uploaderID string) (map[string]bool, error) {
	type set struct {
		ID string `json:"id"`
	}
	sets, err := listAll[set](c, "contents_sets", "uploader:each ?= "+strconv.Quote(uploaderID), "created", "id")
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(sets))
	for _, s := range sets {
		out[s.ID] = true
	}
	return out, nil
}

// recredit points one post at another uploader. A normal record update, so
// hooks/ownership.go re-derives the post's set's uploaders once it commits.
func (c *pbClient) recredit(postID, uploaderID string) error {
	return c.do(http.MethodPatch, "/api/collections/contents/records/"+url.PathEscape(postID),
		map[string]string{"uploader": uploaderID}, nil)
}
