package bot

import (
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// Metadata is everything parsed from a Discord message (key: value lines and
// pinged roles) that applies to every media item found in that message.
type Metadata struct {
	MessageID string
	AuthorID  string

	Title    string
	Idol     string // comma-separated idol names
	Group    string // comma-separated group names
	Tags     string // comma-separated tag names
	Uploader string
	Date     string // "now", "today", YYMMDD, or anything PocketBase accepts
	Source   string
	Filetype string // optional override applied to every item (e.g. "sticker")
	Mirror   string // optional mirror override (for attachment + mirror-line posts)
	Discord  string
	SetId    string
}

// IdolGroup is one idol/group pair parsed from an "Idol [Group]" role name.
type IdolGroup struct {
	Idol  string
	Group string
}

var (
	// roleRegexp parses "Idol Name [GROUP]" role names. `.+?` (not `\w+`) so
	// multi-word idol names ("Chae Won [LE SSERAFIM]") stay intact.
	roleRegexp    = regexp.MustCompile(`^(.+?) \[([^\]]+)\]$`)
	youtubeRegexp = regexp.MustCompile(`(?:https?://)?(?:www\.)?(?:youtube\.com/watch\?v=|youtu\.be/)[\w\-]{11}`)
)

// metadataFields maps "key: value" line keys in a Discord message to the
// Metadata fields they populate.
func metadataFields(m *Metadata) map[string]*string {
	return map[string]*string{
		"filetype": &m.Filetype,
		"title":    &m.Title,
		"idol":     &m.Idol,
		"group":    &m.Group,
		"tags":     &m.Tags,
		"uploader": &m.Uploader,
		"date":     &m.Date,
		"source":   &m.Source,
		"mirror":   &m.Mirror,
	}
}

// extractMetadata parses "key: value" lines from the message into metadata.
// Idol and group are required (from lines or pinged roles); title falls back
// to "<idol> from <group>", and source falls back to the first YouTube link
// found anywhere in the message.
func extractMetadata(content string, metadata *Metadata) error {
	fields := metadataFields(metadata)
	for _, line := range strings.Split(content, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		if field, ok := fields[strings.ToLower(strings.TrimSpace(key))]; ok {
			*field = strings.TrimSpace(value)
		}
	}

	var missing []string
	if metadata.Idol == "" {
		missing = append(missing, "idol")
	}
	if metadata.Group == "" {
		missing = append(missing, "group")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required metadata: %s (add `idol: Name` / `group: Name` lines or ping `Idol [Group]` roles)",
			strings.Join(missing, ", "))
	}

	if metadata.Title == "" {
		metadata.Title = autoTitle(metadata.Idol, metadata.Group)
	}

	// `source:` and `mirror:` land in url-typed fields, and PocketBase rejects
	// the whole record when either isn't one — which took every item in the
	// message down with "source: Must be a valid url." for a post that said
	// `source: my cam`. That line is a note, not a link; keep the note out of the
	// field rather than let it sink the ingestion. Logged, so the dropped text
	// is still findable when someone asks where their source went.
	if metadata.Source != "" && !isHTTPURL(metadata.Source) {
		slog.Info("ignoring source line that is not a URL", "source", metadata.Source)
		metadata.Source = ""
	}
	if metadata.Mirror != "" && !isHTTPURL(metadata.Mirror) {
		slog.Info("ignoring mirror line that is not a URL", "mirror", metadata.Mirror)
		metadata.Mirror = ""
	}

	if metadata.Source == "" {
		// The regexp accepts a schemeless "youtu.be/..." because that is how
		// people paste them; the url field does not, so give it one.
		if yt := youtubeRegexp.FindString(content); yt != "" {
			if !strings.HasPrefix(yt, "http") {
				yt = "https://" + yt
			}
			metadata.Source = yt
		}
	}

	return nil
}

// isHTTPURL reports whether v is an absolute http(s) URL — what PocketBase's
// url field type will accept.
func isHTTPURL(v string) bool {
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// autoTitle is the title given to a post that didn't state one.
//
// "Idols - Groups", not "Idols from Groups": this string ends up as the record's
// title and therefore as the embed heading on the site, where the dash reads as
// a label rather than a sentence fragment.
//
// Shared with runIngestion, which rebuilds the title from the names that
// actually resolved — comparing against this is how it tells a generated title
// from one the poster wrote.
func autoTitle(idol, group string) string {
	if idol == "" && group == "" {
		return ""
	}
	return idol + " - " + group
}

// stripMetadataLines removes recognized "key: value" lines from the content.
// Media extraction runs on the result, so a `mirror:` or `source:` attribution
// link is never ingested as an item of its own.
func stripMetadataLines(content string) string {
	fields := metadataFields(&Metadata{})
	var kept []string
	for _, line := range strings.Split(content, "\n") {
		if key, _, found := strings.Cut(line, ":"); found {
			if _, ok := fields[strings.ToLower(strings.TrimSpace(key))]; ok {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// rolesToMetadata seeds a Metadata with the idol/group names parsed from the
// pinged role names (e.g. "Yujin [IVE]").
func rolesToMetadata(pingRoleNames []string) Metadata {
	idolGroups := extractIdolAndGroupFromRoles(pingRoleNames)
	idolNames, groupNames := joinIdolAndGroupNames(idolGroups)

	return Metadata{
		Idol:  idolNames,
		Group: groupNames,
	}
}

// extractIdolAndGroupFromRoles parses "Idol [Group]" role names into pairs.
// Roles that don't match the pattern are ignored.
func extractIdolAndGroupFromRoles(roleNames []string) []IdolGroup {
	var result []IdolGroup
	for _, roleName := range roleNames {
		matches := roleRegexp.FindStringSubmatch(strings.TrimSpace(roleName))
		if len(matches) == 3 {
			result = append(result, IdolGroup{
				Idol:  matches[1],
				Group: matches[2],
			})
		}
	}
	return result
}

// joinIdolAndGroupNames deduplicates the idol and group names and joins each
// as a comma-separated string.
func joinIdolAndGroupNames(idolGroups []IdolGroup) (string, string) {
	var idolNames, groupNames []string
	idolSeen := make(map[string]bool)
	groupSeen := make(map[string]bool)

	for _, ig := range idolGroups {
		if !idolSeen[ig.Idol] {
			idolSeen[ig.Idol] = true
			idolNames = append(idolNames, ig.Idol)
		}
		if !groupSeen[ig.Group] {
			groupSeen[ig.Group] = true
			groupNames = append(groupNames, ig.Group)
		}
	}

	return strings.Join(idolNames, ", "), strings.Join(groupNames, ", ")
}

// splitTrim splits a comma-separated string into trimmed, non-empty parts.
func splitTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
