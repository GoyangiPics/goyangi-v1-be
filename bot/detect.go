package bot

import (
	"os"
	"regexp"
	"strings"
)

// Free-text idol/group detection: the last-resort trigger, for messages that
// carry media and name an idol but have no role ping and no @-mention.
//
// The reasons this exists, from the people posting: there aren't enough ping
// roles to cover every idol, some deliberately don't ping so as not to alert a
// channel, and some simply forget. None of those leave any trace in the logs —
// an unpinged message is invisible — so unlike the other ingestion rules this
// one is aimed at a problem whose size we can't measure yet. Every detection
// therefore logs what it matched, so the hit rate can be read back out of
// system_logs and the rules tuned.
//
// It is the loosest rule in the codebase, so it is also the strictest about
// what counts:
//
//   - Only `name` and the explicit `aliases` field are matched. Nothing is
//     inferred by splitting names into tokens — "Eunbi" resolving to Kwon Eunbi
//     is an alias somebody entered, not a guess the bot made.
//   - Matches must fall on word boundaries, so "Rei" doesn't fire inside
//     "reinstall".
//   - URLs are stripped first. A filename in a link is not a statement about
//     who is in the media.
//   - An idol is required. A group alone can't produce a record (both relations
//     are mandatory), and "IVE posted today" is not an attribution.
//   - An idol name shared by several groups is only accepted when one of those
//     groups is also named in the message.
//
// Short names are the known weak point — a directory containing Ian, Liz or Rei
// will collide with ordinary English. GOYANGI_DETECT_STOPWORDS suppresses
// specific strings without a deploy.

// detectMinNameLen is the shortest name or alias that may match. Two-character
// names are indistinguishable from noise in running text.
const detectMinNameLen = 3

// urlRegexp strips links before scanning, so a slug like
// imgur.com/kwon-eunbi-AbCd can't attribute a post by itself.
var urlRegexp = regexp.MustCompile(`https?://\S+`)

// detectStopwords are names the matcher must ignore, from
// GOYANGI_DETECT_STOPWORDS (comma-separated, case-insensitive). For directory
// entries whose name is also a common word.
var detectStopwords = func() map[string]bool {
	out := map[string]bool{}
	for _, word := range splitTrim(os.Getenv("GOYANGI_DETECT_STOPWORDS")) {
		if word != "" {
			out[strings.ToLower(word)] = true
		}
	}
	return out
}()

// detection is what the matcher found in a message.
type detection struct {
	idolNames  []string
	groupNames []string
	// matched records which string produced each hit, for the audit log — the
	// alias that fired is the useful bit when tuning, not the canonical name.
	matched []string
}

// detectSubjects scans message text for idols and groups from the directory.
//
// Returns ok=false unless the result is confident enough to attribute media to,
// per the rules documented above.
func detectSubjects(dir *directory, content string) (detection, bool) {
	haystack := " " + strings.ToLower(urlRegexp.ReplaceAllString(stripMetadataLines(content), " ")) + " "

	var (
		found      detection
		seenIdol   = map[string]bool{}
		seenGroup  = map[string]bool{}
		groupHits  = map[string]bool{} // group id -> named in the message
		idolHits   []idolEntry
		addMatched = func(s string) { found.matched = append(found.matched, s) }
	)

	for _, g := range dir.groups {
		if hit, ok := firstNameHit(haystack, g.name, g.aliases); ok {
			groupHits[g.id] = true
			if !seenGroup[g.name] {
				seenGroup[g.name] = true
				found.groupNames = append(found.groupNames, g.name)
			}
			addMatched(hit)
		}
	}

	for _, idol := range dir.idols {
		if hit, ok := firstNameHit(haystack, idol.name, idol.aliases); ok {
			idolHits = append(idolHits, idol)
			addMatched(hit)
		}
	}

	// Ambiguity: the same idol name in several groups is only usable when the
	// message also names one of them. Without that there is no way to pick, and
	// guessing writes the wrong attribution into the archive.
	byName := map[string][]idolEntry{}
	for _, idol := range idolHits {
		byName[strings.ToLower(idol.name)] = append(byName[strings.ToLower(idol.name)], idol)
	}

	for _, idol := range idolHits {
		candidates := byName[strings.ToLower(idol.name)]
		if len(candidates) > 1 && !groupHits[idol.groupID] {
			continue
		}
		if seenIdol[idol.name] {
			continue
		}
		seenIdol[idol.name] = true
		found.idolNames = append(found.idolNames, idol.name)

		// A matched idol implies its group, which is what makes a record
		// possible when only the idol was named.
		if idol.groupName != "" && !seenGroup[idol.groupName] {
			seenGroup[idol.groupName] = true
			found.groupNames = append(found.groupNames, idol.groupName)
		}
	}

	if len(found.idolNames) == 0 || len(found.groupNames) == 0 {
		return detection{}, false
	}
	return found, true
}

// firstNameHit reports the first of a record's names to appear in the haystack.
func firstNameHit(haystack, name string, aliases []string) (string, bool) {
	for _, candidate := range append([]string{name}, aliases...) {
		candidate = strings.TrimSpace(candidate)
		if len([]rune(candidate)) < detectMinNameLen || detectStopwords[strings.ToLower(candidate)] {
			continue
		}
		if containsWord(haystack, strings.ToLower(candidate)) {
			return candidate, true
		}
	}
	return "", false
}

// containsWord looks for a needle bounded by non-word characters on both sides.
//
// Written by hand rather than with a compiled-per-call regexp: this runs over
// every name in the directory for every candidate message, and the names come
// from user data, which would have to be escaped anyway.
func containsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for offset := 0; ; {
		idx := strings.Index(haystack[offset:], needle)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(needle)
		if !isWordByte(haystack[start-1]) && !isWordByte(haystack[end]) {
			return true
		}
		offset = start + 1
	}
}

// isWordByte treats letters, digits and underscore as "inside a word". The
// haystack is padded with spaces by the caller, so index -1 and len are never
// reached.
func isWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_':
		return true
	default:
		return false
	}
}
