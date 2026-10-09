package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"goyangi-v1-be/bot"
	"goyangi-v1-be/hooks"
)

// PocketBase over HTTP, as a superuser. Same shape as scripts/backfilldims;
// superuser rather than a canUpload account because the uploaders create rule
// only lets a caller create a record for themself, and tags are admin-only.

// pbTime is the layout PocketBase itself emits for date fields. Written back in
// the same shape so `created` and `date` round-trip exactly.
const pbTime = "2006-01-02 15:04:05.000Z"

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
func (c *pbClient) do(method, path string, body any, out any) error {
	return c.doWith(method, path, nil, body, out)
}

func (c *pbClient) doWith(method, path string, headers map[string]string, body any, out any) error {
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if resp.StatusCode == http.StatusBadRequest {
			if fields := invalidFields(raw); len(fields) > 0 {
				return &pbValidationError{
					fields: fields,
					err:    fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw))),
				}
			}
		}
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// pbValidationError is a 400 whose body names the field(s) that failed
// validation, so a caller can react to which field rather than string-matching
// the message.
type pbValidationError struct {
	fields []string
	err    error
}

func (e *pbValidationError) Error() string { return e.err.Error() }

// invalidFields extracts the top-level field names PocketBase's validation
// error names, from its standard {"data":{"<field>":{"code":...}}} shape.
// Nested relation-field errors are ignored — none of this script's optional,
// droppable fields nest.
func invalidFields(body []byte) []string {
	var parsed struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	fields := make([]string, 0, len(parsed.Data))
	for k := range parsed.Data {
		fields = append(fields, k)
	}
	return fields
}

type pbRecord map[string]any

func (r pbRecord) str(key string) string {
	v, _ := r[key].(string)
	return v
}

func (r pbRecord) boolean(key string) bool {
	v, _ := r[key].(bool)
	return v
}

// listAll pages through a whole collection (or a filtered slice of it).
func (c *pbClient) listAll(collection, filter, fields string) ([]pbRecord, error) {
	var all []pbRecord
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("perPage", "500")
		q.Set("sort", "created")
		if filter != "" {
			q.Set("filter", filter)
		}
		if fields != "" {
			q.Set("fields", fields)
		}
		var out struct {
			TotalPages int        `json:"totalPages"`
			Items      []pbRecord `json:"items"`
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

// first returns the first record matching filter, or nil.
func (c *pbClient) first(collection, filter, fields string) (pbRecord, error) {
	q := url.Values{}
	q.Set("perPage", "1")
	q.Set("skipTotal", "1")
	q.Set("filter", filter)
	if fields != "" {
		q.Set("fields", fields)
	}
	var out struct {
		Items []pbRecord `json:"items"`
	}
	if err := c.do(http.MethodGet, "/api/collections/"+collection+"/records?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Items) == 0 {
		return nil, nil
	}
	return out.Items[0], nil
}

// droppableOnValidation are fields this script sets from parsed message
// content — a source: line, an auto-detected YouTube link, an imgur mirror —
// rather than from the item itself. bot.ExtractMetadata already discards a
// source/mirror value that fails Go's own (lenient) URL parse, but PocketBase's
// field validator is stricter, so some values pass the first check and fail the
// second. Losing the attribution on one bad value is better than losing the
// whole set over it, the same trade extractMetadata already makes.
var droppableOnValidation = map[string]bool{"source": true, "mirror": true}

// create writes a record. `created` is sent in hooks.CreatedHeader — the body
// can't carry it (see hooks/provenance.go) — and `origin` is claimed in the
// body; both are honoured only for a superuser, only on the server that has the
// provenance hook deployed.
//
// A validation failure on a droppable field is retried once with that field
// cleared, logged so the drop is discoverable. Any other validation failure —
// or a second one after the retry — returns as normal.
func (c *pbClient) create(collection string, data map[string]any, created time.Time, origin string) (pbRecord, error) {
	data["origin"] = origin
	headers := map[string]string{hooks.CreatedHeader: created.UTC().Format(pbTime)}

	var out pbRecord
	err := c.doWith(http.MethodPost, "/api/collections/"+collection+"/records", headers, data, &out)
	if err == nil {
		return out, nil
	}

	var verr *pbValidationError
	if !errors.As(err, &verr) {
		return nil, err
	}
	dropped := false
	for _, f := range verr.fields {
		if droppableOnValidation[f] && data[f] != nil && data[f] != "" {
			log.Printf("   ⚠️  %s: %q failed server validation (%v) — retrying without it", collection, f, data[f])
			data[f] = ""
			dropped = true
		}
	}
	if !dropped {
		return nil, err
	}

	out = nil
	if retryErr := c.doWith(http.MethodPost, "/api/collections/"+collection+"/records", headers, data, &out); retryErr != nil {
		return nil, retryErr
	}
	return out, nil
}

// createPlain writes a record with no provenance claims (uploaders, tags).
func (c *pbClient) createPlain(collection string, data map[string]any) (pbRecord, error) {
	var out pbRecord
	if err := c.do(http.MethodPost, "/api/collections/"+collection+"/records", data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// get reads one record.
func (c *pbClient) get(collection, id, fields string) (pbRecord, error) {
	var out pbRecord
	q := url.Values{}
	if fields != "" {
		q.Set("fields", fields)
	}
	if err := c.do(http.MethodGet, "/api/collections/"+collection+"/records/"+id+"?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *pbClient) update(collection, id string, data map[string]any) (pbRecord, error) {
	var out pbRecord
	if err := c.do(http.MethodPatch, "/api/collections/"+collection+"/records/"+id, data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *pbClient) delete(collection, id string) error {
	return c.do(http.MethodDelete, "/api/collections/"+collection+"/records/"+id, nil, nil)
}

// pbQuote renders a string for a PocketBase filter expression.
func pbQuote(s string) string {
	return strconv.Quote(s)
}

// ─── Directory ───────────────────────────────────────────────────────────────

// directory is the bot's idol/group/uploader/tag directory, read once over the
// API. Name matching mirrors bot.resolveRelations: case-insensitive, aliases
// count, and an idol named outside the stated group still resolves when the
// name is unambiguous across the whole directory.
type directory struct {
	groups    []dirEntry
	idols     []dirIdol
	uploaders []dirEntry
	tags      map[string]string // lowercased name → id

	groupName map[string]string // id → canonical name
	idolName  map[string]string

	mu sync.Mutex // guards uploaders/tags while lookupOrCreate adds to them

	detectOnce sync.Once
	det        *bot.Detector
}

// detector is the bot's free-text idol detector over this directory, built on
// first use.
func (d *directory) detector() *bot.Detector {
	d.detectOnce.Do(func() {
		groups := make([]bot.DetectGroup, 0, len(d.groups))
		for _, g := range d.groups {
			groups = append(groups, bot.DetectGroup{ID: g.id, Name: g.name, Aliases: g.aliases})
		}
		idols := make([]bot.DetectIdol, 0, len(d.idols))
		for _, i := range d.idols {
			idols = append(idols, bot.DetectIdol{
				ID: i.id, Name: i.name, Aliases: i.aliases,
				GroupID: i.groupID, GroupName: d.groupName[i.groupID],
			})
		}
		d.det = bot.NewDetector(groups, idols)
	})
	return d.det
}

type dirEntry struct {
	id, name string
	aliases  []string
	blocked  bool // uploaders: blockIngest
	optedOut bool // uploaders: skipDiscordImport
}

type dirIdol struct {
	dirEntry
	groupID string
}

func loadDirectory(pb *pbClient) (*directory, error) {
	d := &directory{tags: map[string]string{}, groupName: map[string]string{}, idolName: map[string]string{}}

	groups, err := pb.listAll("groups", "", "id,name,aliases")
	if err != nil {
		return nil, fmt.Errorf("groups: %w", err)
	}
	for _, r := range groups {
		d.groups = append(d.groups, toEntry(r))
		d.groupName[r.str("id")] = strings.TrimSpace(r.str("name"))
	}

	idols, err := pb.listAll("groups_idols", "", "id,name,aliases,group")
	if err != nil {
		return nil, fmt.Errorf("idols: %w", err)
	}
	for _, r := range idols {
		d.idols = append(d.idols, dirIdol{dirEntry: toEntry(r), groupID: r.str("group")})
		d.idolName[r.str("id")] = strings.TrimSpace(r.str("name"))
	}

	uploaders, err := pb.listAll("uploaders", "", "id,name,aliases,blockIngest,skipDiscordImport")
	if err != nil {
		return nil, fmt.Errorf("uploaders: %w", err)
	}
	for _, r := range uploaders {
		e := toEntry(r)
		e.blocked = r.boolean("blockIngest")
		e.optedOut = r.boolean("skipDiscordImport")
		d.uploaders = append(d.uploaders, e)
	}

	tags, err := pb.listAll("tags", "", "id,name")
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	for _, r := range tags {
		d.tags[strings.ToLower(strings.TrimSpace(r.str("name")))] = r.str("id")
	}
	return d, nil
}

func toEntry(r pbRecord) dirEntry {
	return dirEntry{
		id:      r.str("id"),
		name:    strings.TrimSpace(r.str("name")),
		aliases: bot.SplitTrim(r.str("aliases")),
	}
}

// resolved is what a message's stated names came to.
type resolved struct {
	idolIDs, groupIDs []string
	missing           []string
}

// resolve is bot.resolveRelations' idol/group half.
func (d *directory) resolve(idolNames, groupNames []string) resolved {
	var out resolved
	groupSet := map[string]bool{}

	for _, n := range groupNames {
		needle := strings.ToLower(strings.TrimSpace(n))
		found := false
		for _, g := range d.groups {
			if bot.MatchesAnyName(needle, g.name, g.aliases) {
				if !groupSet[g.id] {
					groupSet[g.id] = true
					out.groupIDs = append(out.groupIDs, g.id)
				}
				found = true
			}
		}
		if !found {
			out.missing = append(out.missing, "group "+n)
		}
	}

	for _, n := range idolNames {
		needle := strings.ToLower(strings.TrimSpace(n))
		var inGroup, anywhere []dirIdol
		for _, idol := range d.idols {
			if !bot.MatchesAnyName(needle, idol.name, idol.aliases) {
				continue
			}
			anywhere = append(anywhere, idol)
			if groupSet[idol.groupID] {
				inGroup = append(inGroup, idol)
			}
		}
		switch {
		case len(inGroup) > 0:
			out.idolIDs = append(out.idolIDs, inGroup[0].id)
		case len(anywhere) == 1:
			// The stated group didn't contain her; unambiguous, so she wins and
			// brings her real group along (see bot.resolveRelations).
			out.idolIDs = append(out.idolIDs, anywhere[0].id)
			if g := anywhere[0].groupID; g != "" && !groupSet[g] {
				groupSet[g] = true
				out.groupIDs = append(out.groupIDs, g)
			}
		default:
			out.missing = append(out.missing, "idol "+n)
		}
	}
	return out
}

// names maps relation ids back to canonical names, in order.
func (d *directory) names(byID map[string]string, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if n := byID[id]; n != "" {
			out = append(out, n)
		}
	}
	return out
}

// lookupOrCreateUploader is bot.lookupOrCreateByName for uploaders: name or
// alias, case-insensitive, created when unknown. skipReason is non-empty when
// the uploader is blocked or has turned off automatic import, so the caller can
// abandon the message the way the live bot's automatic path does — everything
// this script fills in is what that path would have ingested.
func (d *directory) lookupOrCreateUploader(pb *pbClient, name string, commit bool) (id, skipReason string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	needle := strings.ToLower(strings.TrimSpace(name))
	for _, u := range d.uploaders {
		if bot.MatchesAnyName(needle, u.name, u.aliases) {
			switch {
			case u.blocked:
				return u.id, "is blocked from ingestion", nil
			case u.optedOut:
				return u.id, "turned off automatic import from Discord", nil
			}
			return u.id, "", nil
		}
	}
	if !commit {
		return "(new uploader)", "", nil
	}
	rec, err := pb.createPlain("uploaders", map[string]any{"name": strings.TrimSpace(name)})
	if err != nil {
		return "", "", err
	}
	d.uploaders = append(d.uploaders, dirEntry{id: rec.str("id"), name: strings.TrimSpace(name)})
	return rec.str("id"), "", nil
}

// lookupOrCreateTag mirrors the tag half of bot.lookupOrCreateByName, including
// the derived `code`.
func (d *directory) lookupOrCreateTag(pb *pbClient, name string, commit bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	trimmed := strings.TrimSpace(name)
	needle := strings.ToLower(trimmed)
	if id, ok := d.tags[needle]; ok {
		return id, nil
	}
	if !commit {
		return "(new tag)", nil
	}
	code := hooks.Slugify(trimmed)
	if code == "" {
		code = needle
	}
	rec, err := pb.createPlain("tags", map[string]any{"name": trimmed, "code": code})
	if err != nil {
		return "", err
	}
	d.tags[needle] = rec.str("id")
	return rec.str("id"), nil
}
