package bot

import (
	"strings"

	"github.com/disgoorg/disgo/discord"
)

// The message-parsing half of ingestion, exported for scripts/backfilldiscord.
//
// The backfill replays historical messages through the same decisions the live
// bot makes — which links count as media, how an imgur link is normalised, how
// `key: value` lines and "Idol [Group]" roles become metadata, who is credited.
// Those live here as unexported functions because nothing outside the package
// needed them; the script does, and copying the regexes into it would give the
// archive two definitions of "a media link" that drift apart. So: thin exported
// names over the existing functions, and nothing else. No behaviour lives in
// this file.

// CollectMedia is collectMedia: every ingestible attachment and link in a
// message, attribution lines stripped first.
func CollectMedia(m discord.Message) []MediaItem { return collectMedia(m) }

// RolesToMetadata is rolesToMetadata: idol and group names from pinged
// "Idol [Group]" role names.
func RolesToMetadata(roleNames []string) Metadata { return rolesToMetadata(roleNames) }

// ExtractMetadata is extractMetadata: applies the message's `key: value` lines,
// requires idol and group, derives the title and the YouTube source.
func ExtractMetadata(content string, m *Metadata) error { return extractMetadata(content, m) }

// AutoTitle is autoTitle — the generated "Idols - Groups" title, which callers
// compare against to tell a generated title from a stated one.
func AutoTitle(idol, group string) string { return autoTitle(idol, group) }

// UploaderFromMessage is uploaderFromMessage's name half: the invoking user for
// a relayed post, otherwise the author's username.
func UploaderFromMessage(m discord.Message) string {
	name, _ := uploaderFromMessage(m)
	return name
}

// SplitTrim is splitTrim.
func SplitTrim(s string) []string { return splitTrim(s) }

// MatchesAnyName is matchesAnyName: needle (already lowercased) against a
// record's name and aliases.
func MatchesAnyName(needle, name string, aliases []string) bool {
	return matchesAnyName(needle, name, aliases)
}

// BotMentionedIn is botIsMentioned's content check for a message that didn't
// arrive as a gateway event: a literal <@id> in the text, so a reply that merely
// pings the bot's message doesn't count.
func BotMentionedIn(content, appID string) bool {
	return appID != "" &&
		(strings.Contains(content, "<@"+appID+">") || strings.Contains(content, "<@!"+appID+">"))
}

// The bare follow-up rule's two bounds, for a replay of it (see state.go for
// why they are what they are).
const (
	ChainWindow       = chainWindow
	ChainMaxFollowUps = chainMaxFollowUps
)

// DetectGroup and DetectIdol are the directory entries text detection scans,
// for a caller that read the directory over the API instead of from the
// database loadDirectory reads.
type DetectGroup struct {
	ID, Name string
	Aliases  []string
}

type DetectIdol struct {
	ID, Name, GroupID, GroupName string
	Aliases                      []string
}

// Detector is detectSubjects over a directory the caller built, so a replay of
// textDetection uses the bot's matching rules and not a copy of them.
type Detector struct{ dir *directory }

func NewDetector(groups []DetectGroup, idols []DetectIdol) *Detector {
	dir := &directory{}
	for _, g := range groups {
		dir.groups = append(dir.groups, groupEntry{id: g.ID, name: g.Name, aliases: g.Aliases})
	}
	for _, i := range idols {
		dir.idols = append(dir.idols, idolEntry{id: i.ID, name: i.Name, aliases: i.Aliases, groupID: i.GroupID, groupName: i.GroupName})
	}
	return &Detector{dir: dir}
}

// Detect returns the idol and group names the text names and the strings that
// matched, ok=false unless detectSubjects is confident enough to attribute to.
func (d *Detector) Detect(content string) (idols, groups, matched []string, ok bool) {
	found, ok := detectSubjects(d.dir, content)
	return found.idolNames, found.groupNames, found.matched, ok
}

// DetectStopwordCount is how many names GOYANGI_DETECT_STOPWORDS suppressed
// when the process started, so a replay can say whether it has the list.
func DetectStopwordCount() int { return len(detectStopwords) }
