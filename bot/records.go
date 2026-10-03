package bot

import (
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"goyangi-v1-be/hooks"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
)

// resolvedRelations holds the record ids the message's names resolved to,
// plus the names that didn't resolve (surfaced back to the poster instead of
// being dropped silently — idol/group are required fields, so a content
// record with unresolved names would fail validation anyway).
type resolvedRelations struct {
	idolIDs     []string
	groupIDs    []string
	uploaderIDs []string
	tagIDs      []string
	missing     []string // "idol Wonyoung", "group NewJeans", ...
	// Names of resolved uploaders carrying `blockIngest`. Non-empty means the
	// run must be abandoned — see runIngestion.
	blockedUploaders []string
}

// resolveRelations maps every name in the metadata to PocketBase record ids.
// Idols and groups must already exist; uploaders and tags are created on the
// fly. Runs once per message — the result is shared by the set record and
// every content record.
func resolveRelations(m Metadata) (resolvedRelations, error) {
	var rel resolvedRelations

	// One directory read serves every idol/group lookup below — the same
	// 60s-TTL cache autocomplete, /random and text detection already use —
	// instead of a full-collection scan per name (a five-name message used to
	// cost five scans of each table). The TTL means a group or alias edited
	// seconds ago can take up to a minute to resolve; the real-world
	// fix-the-alias-and-repost loop runs on minutes, so the trade holds here
	// too. On a load error, degrade exactly like the old per-name scans did:
	// nothing resolves and every name reports missing.
	dir, err := loadDirectory()
	if err != nil {
		dir = &directory{}
	}

	groupIDSet, missingGroups := findGroupIDsByNames(dir, splitTrim(m.Group))
	for id := range groupIDSet {
		rel.groupIDs = append(rel.groupIDs, id)
	}
	for _, name := range missingGroups {
		rel.missing = append(rel.missing, "group "+name)
	}

	for _, name := range splitTrim(m.Idol) {
		if id, ok := findIdolIDByGroupIDs(dir, name, groupIDSet); ok {
			rel.idolIDs = append(rel.idolIDs, id)
			continue
		}
		// The stated group didn't contain this idol — usually because the ping
		// names the group she debuted with rather than where she sits now:
		// "Eunbi [IZONE]" for a record filed under Solo, "Yuju [GFRIEND]" the
		// same. The idol is the more specific signal, so when the name resolves
		// to exactly one person in the whole directory, take her and her real
		// group rather than dropping the message.
		//
		// Only when unambiguous. If two idols share the name, the stated group
		// is the only thing that could have told them apart, and guessing would
		// file the media under the wrong person.
		if id, groupID, ok := findIdolIDByNameAnywhere(dir, name); ok {
			rel.idolIDs = append(rel.idolIDs, id)
			if groupID != "" && !groupIDSet[groupID] {
				groupIDSet[groupID] = true
				rel.groupIDs = append(rel.groupIDs, groupID)
			}
			slog.Info("idol resolved outside the stated group", "idol", name, "stated", m.Group)
			continue
		}
		rel.missing = append(rel.missing, "idol "+name)
	}

	for _, name := range splitTrim(m.Uploader) {
		id, err := lookupOrCreateByName("uploaders", name)
		if err != nil {
			return rel, fmt.Errorf("uploader %q: %w", name, err)
		}
		rel.uploaderIDs = append(rel.uploaderIDs, id)
		// Checked here rather than at the call site so every entry point gets it
		// for free — passive ingestion, the context command and /reupload all
		// funnel through resolveRelations.
		if blocked, bname := uploaderBlocksIngest(id); blocked {
			rel.blockedUploaders = append(rel.blockedUploaders, bname)
		}
	}

	for _, name := range splitTrim(m.Tags) {
		id, err := lookupOrCreateByName("tags", name)
		if err != nil {
			return rel, fmt.Errorf("tag %q: %w", name, err)
		}
		rel.tagIDs = append(rel.tagIDs, id)
	}

	return rel, nil
}

// uploaderBlocksIngest reports whether this uploader is flagged to have its
// ingestion dropped, and its name for the log.
//
// Read live rather than through the directory cache: flipping the flag in the
// admin UI has to take effect on the next message, not up to a minute later.
// It is one lookup per uploader named on a message, which is almost always one.
func uploaderBlocksIngest(id string) (blocked bool, name string) {
	record, err := App.FindRecordById("uploaders", id)
	if err != nil {
		// Fail OPEN: a lookup failure must not silently start dropping
		// everybody's uploads. The flag is a quality filter, and losing content
		// to a transient database error is the worse outcome.
		slog.Warn("could not read uploader for the ingest block check", "uploader", id, "err", err)
		return false, ""
	}
	if !record.GetBool("blockIngest") {
		return false, ""
	}
	return true, record.GetString("name")
}

// findGroupIDsByNames resolves group names against the directory
// (case-insensitive, aliases counted — see the aliases note below) and reports
// which names found no match. Names are matched in Go rather than in a filter
// string so user-supplied text can't inject into the query.
//
// A name matching several groups keeps ALL of them, matching the old
// whole-table scan: an alias deliberately shared by two group records means
// both, not whichever the loop saw first.
//
// Aliases exist because what people ping and type is rarely the canonical
// name. "Eunbi" for Kwon Eunbi and "RcPc" for Rocket Punch were the two
// biggest sources of dropped ingestions in system_logs — the poster did
// everything right and the string simply didn't match. Keeping them as data
// means the mod team fixes a miss by editing a record, not by shipping a
// deploy.
func findGroupIDsByNames(dir *directory, names []string) (map[string]bool, []string) {
	set := make(map[string]bool)
	var missing []string
	for _, n := range names {
		needle := strings.ToLower(strings.TrimSpace(n))
		found := false
		for _, g := range dir.groups {
			if matchesAnyName(needle, g.name, g.aliases) {
				set[g.id] = true
				found = true
			}
		}
		if !found {
			missing = append(missing, n)
		}
	}
	return set, missing
}

// findIdolIDByGroupIDs resolves an idol name within the given group IDs.
func findIdolIDByGroupIDs(dir *directory, idolName string, groupIDSet map[string]bool) (string, bool) {
	if len(groupIDSet) == 0 {
		return "", false
	}
	needle := strings.ToLower(strings.TrimSpace(idolName))
	for _, idol := range dir.idols {
		if !groupIDSet[idol.groupID] {
			continue
		}
		if matchesAnyName(needle, idol.name, idol.aliases) {
			return idol.id, true
		}
	}
	return "", false
}

// findIdolIDByNameAnywhere resolves an idol by name across every group, and
// reports the group she actually belongs to.
//
// ok is false when nobody matches OR when several do — an ambiguous name has no
// safe answer without a group to narrow it.
func findIdolIDByNameAnywhere(dir *directory, idolName string) (id, groupID string, ok bool) {
	needle := strings.ToLower(strings.TrimSpace(idolName))
	var matches []idolEntry
	for _, idol := range dir.idols {
		if matchesAnyName(needle, idol.name, idol.aliases) {
			matches = append(matches, idol)
		}
	}
	if len(matches) != 1 {
		return "", "", false
	}
	return matches[0].id, matches[0].groupID, true
}

// namesByIDs returns the canonical `name` of each id, in the order given.
// Missing ids are skipped rather than yielding blanks. Only the two directory
// tables are answerable; the caller (the retitle step) never needs more.
func namesByIDs(collection string, ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	dir, err := loadDirectory()
	if err != nil {
		return nil
	}
	byID := make(map[string]string)
	switch collection {
	case "groups":
		for _, g := range dir.groups {
			byID[g.id] = g.name
		}
	case "groups_idols":
		for _, idol := range dir.idols {
			byID[idol.id] = idol.name
		}
	default:
		slog.Error("namesByIDs: unsupported collection", "collection", collection)
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if name := byID[id]; name != "" {
			out = append(out, name)
		}
	}
	return out
}

// recordNames is every string a record answers to, lowercased: its `name` plus
// any comma-separated `aliases`.
//
// Aliases exist because what people ping and type is rarely the canonical name.
// "Eunbi" for Kwon Eunbi and "RcPc" for Rocket Punch were the two biggest sources
// of dropped ingestions in system_logs — the poster did everything right and the
// string simply didn't match. Keeping them as data means the mod team fixes a
// miss by editing a record, not by shipping a deploy. Uploaders carry the field
// for the same reason (see hooks/uploaders.go); a collection without it just
// answers to its name.
func recordNames(r *core.Record) []string {
	names := []string{strings.ToLower(strings.TrimSpace(r.GetString("name")))}
	for _, alias := range splitTrim(r.GetString("aliases")) {
		if lowered := strings.ToLower(strings.TrimSpace(alias)); lowered != "" {
			names = append(names, lowered)
		}
	}
	return names
}

// lookupOrCreateByName returns the id of the record in the collection with
// the given name (case-insensitive), creating it if it doesn't exist yet.
// Used for uploaders and tags.
//
// Matching goes through recordNames, so an `aliases` entry counts as the record's
// name. That is what keeps one human from becoming two uploaders: the poster's
// Discord username rarely equals the name their site account chose, and without
// aliases this created a second uploader record for them — one carrying no
// `user`, so their Discord-ingested content was neither editable by them nor
// listed in their own uploads. Putting the Discord name in the site uploader's
// aliases resolves both doors onto one record. `tags` has no aliases field and
// simply keeps matching on name.
func lookupOrCreateByName(collection, name string) (string, error) {
	// Serialize so two concurrent messages don't both create the same record.
	createByNameMu.Lock()
	defer createByNameMu.Unlock()

	records, err := App.FindRecordsByFilter(collection, "", "-created", 0, 0)
	if err == nil {
		needle := strings.ToLower(strings.TrimSpace(name))
		for _, r := range records {
			for _, candidate := range recordNames(r) {
				if candidate == needle {
					return r.Id, nil
				}
			}
		}
	}

	col, err := App.FindCollectionByNameOrId(collection)
	if err != nil {
		return "", err
	}

	trimmed := strings.TrimSpace(name)
	record := core.NewRecord(col)
	record.Set("name", trimmed)

	// `tags` has a required `code`; `uploaders` has no such field. Without this,
	// creating a genuinely new tag failed validation, resolveRelations turned
	// that into a hard error, and runIngestion rejected the ENTIRE message — one
	// unknown tag lost every item in the post.
	//
	// Derived from the name rather than branching per collection at the call
	// sites. Slugify can return "" (a name with no alphanumerics, e.g. "!!!"),
	// which would fail the same required check, so fall back to the trimmed name.
	if col.Fields.GetByName("code") != nil {
		code := hooks.Slugify(trimmed)
		if code == "" {
			code = strings.ToLower(trimmed)
		}
		record.Set("code", code)
	}

	if err := App.Save(record); err != nil {
		return "", err
	}

	return record.Id, nil
}

// createSetRecord creates the "contents_sets" record that groups a multi-item
// Discord message and returns the id it was saved under. The id is assigned
// by the R2 hook's OnRecordCreate("contents_sets") (date-group-idol-suffix),
// so it MUST be read back from the saved record — a pre-generated id would be
// overwritten and the contents' "set" relation would dangle.
func createSetRecord(metadata Metadata, rel resolvedRelations) (string, error) {
	// The set title is prefixed with the YYMMDD date, defaulting to today when
	// the supplied date is missing or malformed.
	date := time.Now().Format("060102")
	if t, err := time.Parse("060102", metadata.Date); err == nil {
		date = t.Format("060102")
	}

	collection, err := App.FindCollectionByNameOrId("contents_sets")
	if err != nil {
		return "", err
	}

	record := core.NewRecord(collection)
	record.Set("title", fmt.Sprintf("%s %s", date, metadata.Title))
	record.Set("idol", rel.idolIDs)
	record.Set("group", rel.groupIDs)
	record.Set("uploader", rel.uploaderIDs)
	// Explicit because contents_sets has no `discord` field for the origin hook
	// to derive from — every set the bot creates came from Discord by definition.
	record.Set("origin", hooks.OriginDiscord)
	if d := normalizeDate(metadata.Date); d != "" {
		record.Set("date", d)
	}

	if err := App.Save(record); err != nil {
		return "", err
	}
	return record.Id, nil
}

// createContentRecord downloads one media item and creates its "contents"
// record, returning the new record id. The R2 hook picks the record up after
// creation and moves/transcodes the file into its public R2 location.
func createContentRecord(item MediaItem, metadata Metadata, rel resolvedRelations) (string, error) {
	data, contentType, suggestedName, err := downloadFile(item.URL)
	if err != nil {
		return "", err
	}

	// Filetype precedence: explicit `filetype:` line > link/attachment
	// classification > HTTP Content-Type of the download.
	filetype := metadata.Filetype
	if filetype == "" {
		filetype = item.Filetype
	}
	if filetype == "" {
		filetype = filetypeByContentType(contentType)
	}
	if filetype == "" {
		return "", fmt.Errorf("cannot determine filetype (content-type %q)", contentType)
	}

	filename := item.Filename
	if suggestedName != "" {
		filename = suggestedName
	}
	// The encode pipeline picks its ffmpeg input format from the extension.
	if path.Ext(filename) == "" {
		if ext := extFromContentType(contentType); ext != "" {
			filename += ext
		}
	}

	mirror := metadata.Mirror
	if mirror == "" {
		mirror = item.Mirror
	}

	collection, err := App.FindCollectionByNameOrId("contents")
	if err != nil {
		return "", err
	}

	record := core.NewRecord(collection)
	record.Set("title", metadata.Title)
	record.Set("idol", rel.idolIDs)
	record.Set("group", rel.groupIDs)
	record.Set("uploader", rel.uploaderIDs)
	// Discord-sourced content defaults to an animated WebP preview rather than
	// AVIF. The encode pipeline reads this field and, unset, would pick AVIF —
	// which not every device on Discord can decode, and these items are seen
	// mostly through Discord embeds.
	record.Set("preview_format", "webp")
	record.Set("tag", rel.tagIDs)
	record.Set("filetype", filetype)
	record.Set("date", normalizeDate(metadata.Date))
	record.Set("source", metadata.Source)
	record.Set("discord", metadata.Discord)
	record.Set("mirror", mirror)
	// The name the file arrived with — see contents.filename on the schema.
	record.Set("filename", filename)
	record.Set("set", metadata.SetId)

	file, err := filesystem.NewFileFromBytes(data, filename)
	if err != nil {
		return "", err
	}
	record.Set("file", file)

	if err := App.Save(record); err != nil {
		return "", err
	}

	return record.Id, nil
}

// normalizeDate converts a Discord-supplied date ("now", "today", or YYMMDD)
// to RFC3339; anything else is passed through for PocketBase to validate.
func normalizeDate(d string) string {
	if d == "now" || d == "today" {
		return time.Now().Format(time.RFC3339Nano)
	}
	if t, err := time.Parse("060102", d); err == nil {
		return t.Format(time.RFC3339Nano)
	}
	return d
}

// maxDownloadBytes caps how much we'll pull from a remote source. Matches the
// 100 MB per-file ceiling the R2 hook enforces at record-create time
// (hooks.maxUploadSize) so oversized downloads fail fast with a clear error
// instead of after the upload attempt.
const maxDownloadBytes = 100 << 20 // 100 MiB

// mediaDownloadClient fetches remote media. The transport refuses private and
// local destinations (see safehttp.go) — the URLs come from channel messages,
// not from us. Shared so downloads also reuse connections.
var mediaDownloadClient = &http.Client{
	Timeout:   3 * time.Minute,
	Transport: publicOnlyTransport(),
}

// downloadFile fetches the media, returning its bytes, the response
// Content-Type, and the filename suggested by Content-Disposition (if any).
func downloadFile(link string) (data []byte, contentType, filename string, err error) {
	req, err := http.NewRequest("GET", link, nil)
	if err != nil {
		return nil, "", "", err
	}

	// Some hosts (imgur) refuse requests without a browser User-Agent.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36")

	resp, err := mediaDownloadClient.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("failed to download file: %s", resp.Status)
	}

	// Read at most maxDownloadBytes+1 so we can detect an over-limit body.
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, "", "", err
	}
	if len(data) > maxDownloadBytes {
		return nil, "", "", fmt.Errorf("file exceeds the %d MB limit", maxDownloadBytes/(1024*1024))
	}
	if len(data) == 0 {
		return nil, "", "", fmt.Errorf("downloaded file is empty")
	}

	if _, params, perr := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); perr == nil {
		filename = path.Base(params["filename"]) // Base() defuses any path components
		if filename == "." || filename == "/" {
			filename = ""
		}
	}

	return data, resp.Header.Get("Content-Type"), filename, nil
}
