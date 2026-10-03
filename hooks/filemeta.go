package hooks

import (
	"os"
	"sort"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// Every MP4 rendition carries the record's provenance inside the file.
//
// The point is to make a downloaded file self-describing: someone with a folder
// of clips and no access to the database can still find "everything uploaded by
// X" or "everything of idol Y" with their file manager's search. So two sets of
// tags are written:
//
//   - The STANDARD container keys — title, artist, date, comment — because those
//     are what Finder, Spotlight, Windows Explorer and every media player index.
//     `artist` carries the uploader precisely so a search by uploader works in
//     tooling that has never heard of goyangi.
//   - `goyangi_*` keys with the same facts spelled out unambiguously, for
//     scripts and for anyone reading the tags with ffprobe or exiftool.
//
// The custom keys only survive with `-movflags use_metadata_tags`; without it
// the mov muxer silently drops anything outside its fixed list. The three mp4
// encoders all pass that flag.
//
// Stills are NOT covered here. ffmpeg's AVIF muxer drops every -metadata tag
// (verified: only the brand atoms survive); AVIF metadata means XMP, which
// avifenc can embed but the still pipeline's ffmpeg encode cannot. That is a
// separate decision — see the pipeline notes.

const uploaderCollectionId = "pbc_2885513906"

// siteBaseURL is what the comment tag links back to.
func siteBaseURL() string {
	if v := strings.TrimRight(os.Getenv("GOYANGI_SITE_URL"), "/"); v != "" {
		return v
	}
	return "https://goyangi.pics"
}

// renditionTags builds the tag set for one record. `n` is the naming already
// resolved for the R2 key, so the idol and group lookups aren't repeated.
func renditionTags(app *pocketbase.PocketBase, record *core.Record, n contentNaming) map[string]string {
	uploaders := strings.Join(
		resolveAllRelationNames(app, record.GetStringSlice("uploader"), uploaderCollectionId), ", ")
	idols := strings.Join(n.idolNames, ", ")
	groups := strings.Join(n.groupNames, ", ")

	tags := map[string]string{
		"title":   record.GetString("title"),
		"artist":  uploaders,
		"comment": siteBaseURL() + "/single/" + record.Id,

		"goyangi_id":       record.Id,
		"goyangi_uploader": uploaders,
		"goyangi_filename": record.GetString("filename"),
		"goyangi_idol":     idols,
		"goyangi_group":    groups,
		"goyangi_uploaded": record.GetDateTime("created").Time().UTC().Format("2006-01-02"),
	}
	// The content's own date, when stated — that is the one a viewer means by
	// "when was this", and the one worth putting under the standard key.
	if d := record.GetDateTime("date"); !d.IsZero() {
		iso := d.Time().UTC().Format("2006-01-02")
		tags["date"] = iso
		tags["goyangi_date"] = iso
	}
	return tags
}

// metadataArgs renders tags as ffmpeg `-metadata key=value` arguments.
//
// Sorted so two encodes of the same record produce byte-identical argument
// lists, and empties skipped so an unset field doesn't write an empty tag that
// tooling then shows as a blank column. Values go through as their own argv
// entries, so commas, quotes and spaces need no escaping.
func metadataArgs(tags map[string]string) []string {
	keys := make([]string, 0, len(tags))
	for k, v := range tags {
		if strings.TrimSpace(v) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		args = append(args, "-metadata", k+"="+tags[k])
	}
	return args
}
