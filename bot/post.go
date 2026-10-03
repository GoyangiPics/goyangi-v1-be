package bot

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	disbot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
	"github.com/pocketbase/pocketbase/core"
)

// One layout serves both ways content reaches a channel: /post, where a human
// picks the pings, and the automatic announcement when something is uploaded on
// the site (autopost.go). Keeping them on the same renderer is the point of the
// exercise — every content post in the server looks the same.

// postPreviewCount is how many preview files ride along under the set link.
// Discord unfurls at most 5 embeds per message and the set link takes one, so
// this is the ceiling rather than a preference.
const postPreviewCount = 4

// postLayout is one content post.
type postLayout struct {
	author   string   // "" posts anonymously
	pings    []string // role mentions, already in <@&id> form
	mirror   string
	source   string
	pageLink string   // the set (or single-item) page — unfurls into its own embed
	previews []string // animated preview URLs, rendered inline
}

// render lays the post out.
//
// The page link goes out bare so Discord unfurls it and the post carries the
// set's own embed. Mirror and Source are masked links, which never unfurl —
// they're references, not content, and each would otherwise burn one of the
// five embed slots.
func (p postLayout) render() string {
	lines := make([]string, 0, 6+len(p.previews))

	if p.author != "" {
		lines = append(lines, "-# by "+p.author)
	}
	if len(p.pings) > 0 {
		lines = append(lines, strings.Join(p.pings, " "))
	}
	if link := maskedLink("Mirror", p.mirror); link != "" {
		lines = append(lines, link)
	}
	if link := maskedLink("Source", p.source); link != "" {
		lines = append(lines, link)
	}
	if p.pageLink != "" {
		lines = append(lines, p.pageLink)
	}
	lines = append(lines, p.previews...)

	return strings.Join(lines, "\n")
}

// maskedLink renders `[label](url)`, falling back to the bare string when it
// isn't an http(s) URL or holds a character that would break the syntax — a
// malformed masked link renders as literal text, which looks broken.
func maskedLink(label, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.ContainsAny(raw, "()<> \t\n") {
		return raw
	}
	if parsed, err := url.Parse(raw); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return raw
	}
	return fmt.Sprintf("[%s](%s)", label, raw)
}

// previewsForPost picks up to n preview files at random from the set.
//
// records must be newest-first, matching the site's own set ordering: the first
// item is the set's cover, so the unfurled set embed already shows it and
// including its preview here would post the same content twice.
func previewsForPost(records []*core.Record, n int) []string {
	if len(records) <= 1 {
		// A single-item set is its own cover — the set embed is the content.
		return nil
	}

	candidates := make([]string, 0, len(records)-1)
	for _, record := range records[1:] {
		if link := mediaLink(record, formatPreview); link != "" {
			candidates = append(candidates, link)
		}
	}
	if len(candidates) <= n {
		return candidates
	}

	picks := make([]string, 0, n)
	for _, idx := range rand.Perm(len(candidates))[:n] {
		picks = append(picks, candidates[idx])
	}
	return picks
}

// ---------------------------------------------------------------------------
// Guild roles
// ---------------------------------------------------------------------------

// Roles back the `pings` autocomplete, which fires on every keystroke, so they
// are cached briefly — a server's role list changes far more slowly than
// somebody types.
const guildRolesTTL = time.Minute

type cachedRoles struct {
	roles  []discord.Role
	loaded time.Time
}

var (
	guildRolesMu    sync.Mutex
	guildRolesCache = map[snowflake.ID]cachedRoles{}
)

func loadGuildRoles(client *disbot.Client, guildID snowflake.ID) ([]discord.Role, error) {
	guildRolesMu.Lock()
	defer guildRolesMu.Unlock()

	if cached, ok := guildRolesCache[guildID]; ok && time.Since(cached.loaded) < guildRolesTTL {
		return cached.roles, nil
	}

	roles, err := client.Rest.GetRoles(guildID)
	if err != nil {
		return nil, err
	}
	guildRolesCache[guildID] = cachedRoles{roles: roles, loaded: time.Now()}
	return roles, nil
}

// findRoleByName matches a role on name, case-insensitively.
func findRoleByName(roles []discord.Role, name string) (discord.Role, bool) {
	needle := strings.ToLower(strings.TrimSpace(name))
	if needle == "" {
		return discord.Role{}, false
	}
	for _, role := range roles {
		if strings.ToLower(role.Name) == needle {
			return role, true
		}
	}
	return discord.Role{}, false
}

// roleMentionIDRegexp matches a pasted `<@&123>` mention.
var roleMentionIDRegexp = regexp.MustCompile(`^<@&(\d+)>$`)

// resolvePings turns the comma-separated `pings` value into role mentions.
//
// Names that match no role are reported rather than dropped: the whole point of
// the command is to notify people, so a ping that silently vanishes is worse
// than being told to fix it.
func resolvePings(client *disbot.Client, guildID snowflake.ID, raw string) (mentions []string, roleIDs []snowflake.ID, missing []string, err error) {
	names := splitTrim(raw)
	if len(names) == 0 {
		return nil, nil, nil, nil
	}

	roles, err := loadGuildRoles(client, guildID)
	if err != nil {
		return nil, nil, nil, err
	}

	seen := make(map[snowflake.ID]bool, len(names))
	for _, name := range names {
		var id snowflake.ID

		// A pasted mention resolves straight to its id.
		if m := roleMentionIDRegexp.FindStringSubmatch(name); m != nil {
			parsed, perr := snowflake.Parse(m[1])
			if perr != nil {
				missing = append(missing, name)
				continue
			}
			id = parsed
		} else if role, ok := findRoleByName(roles, name); ok {
			id = role.ID
		} else {
			missing = append(missing, name)
			continue
		}

		if seen[id] {
			continue
		}
		seen[id] = true
		roleIDs = append(roleIDs, id)
		mentions = append(mentions, discord.RoleMention(id))
	}

	return mentions, roleIDs, missing, nil
}

// allowedRoleMentions restricts a post to exactly the roles it names. Parse is
// explicitly empty so nothing else in the content — @everyone above all — can
// resolve into a ping.
func allowedRoleMentions(roleIDs []snowflake.ID) *discord.AllowedMentions {
	return &discord.AllowedMentions{
		Parse: []discord.AllowedMentionType{},
		Roles: roleIDs,
	}
}

// ---------------------------------------------------------------------------
// `pings` autocomplete
// ---------------------------------------------------------------------------

// maxChoiceValueLength is Discord's cap on an autocomplete choice value. An
// accumulated ping list longer than this can't be offered as a suggestion —
// the user can still type it out, and resolvePings handles it either way.
const maxChoiceValueLength = 100

// splitPingInput divides the option's current text into the part already
// committed (everything before the last comma) and the fragment being typed.
func splitPingInput(input string) (committed, typing string) {
	if idx := strings.LastIndex(input, ","); idx >= 0 {
		return strings.TrimRight(strings.TrimSpace(input[:idx]), " ,"), strings.TrimSpace(input[idx+1:])
	}
	return "", strings.TrimSpace(input)
}

// pingChoices suggests role names, appending to whatever is already in the
// option so a list can be built by picking repeatedly: choose "IVE", type a
// comma, choose "Leeseo [IVE]".
func pingChoices(roles []discord.Role, input string) []discord.AutocompleteChoice {
	committed, typing := splitPingInput(input)
	needle := strings.ToLower(typing)

	already := make(map[string]bool)
	for _, name := range splitTrim(committed) {
		already[strings.ToLower(name)] = true
	}

	choices := make([]discord.AutocompleteChoice, 0, maxAutocompleteChoices)
	for _, role := range roles {
		if len(choices) == maxAutocompleteChoices {
			break
		}
		// @everyone is every guild's default role and must never be a ping.
		if role.Name == "@everyone" || already[strings.ToLower(role.Name)] {
			continue
		}
		if !matchesNeedle(role.Name, needle) {
			continue
		}

		value := role.Name
		if committed != "" {
			value = committed + ", " + role.Name
		}
		if len(value) > maxChoiceValueLength {
			continue
		}
		choices = append(choices, discord.AutocompleteChoiceString{Name: value, Value: value})
	}
	return choices
}

// ---------------------------------------------------------------------------
// /post
// ---------------------------------------------------------------------------

func postCommandCreate() discord.SlashCommandCreate {
	return discord.SlashCommandCreate{
		Name:        "post",
		Description: "Post a goyangi set to this channel, with role pings.",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "link",
				Description: "A goyangi set link (e.g. 'https://goyangi.pics/set/260727-ive-leeseo-b4a29354')",
				Required:    true,
			},
			discord.ApplicationCommandOptionString{
				Name:         "pings",
				Description:  "Roles to ping — pick one, type a comma, pick another",
				Autocomplete: true,
			},
			discord.ApplicationCommandOptionString{
				Name:        "mirror",
				Description: "Mirror link to credit (optional)",
			},
			discord.ApplicationCommandOptionString{
				Name:        "source",
				Description: "Source link to credit (optional)",
			},
			discord.ApplicationCommandOptionBool{
				Name:        "anonymous",
				Description: "Post without the \"by <you>\" byline (default: shown)",
			},
		},
	}
}

func handlePostCommand(e *events.ApplicationCommandInteractionCreate, data discord.SlashCommandInteractionData) {
	setLink := strings.TrimSpace(data.String("link"))
	pings := optString(data, "pings", "")
	mirror := optString(data, "mirror", "")
	source := optString(data, "source", "")
	anonymous := data.Bool("anonymous")

	r := newReply(e)
	if !r.Defer() {
		return
	}

	if e.GuildID() == nil {
		r.Fail("`/post` only works in a server — there are no roles to ping in a DM.")
		return
	}

	ref, err := parseSetLink(setLink)
	if err != nil {
		r.Fail("%s", err.Error())
		return
	}

	records, err := findSetContents(ref)
	if err != nil {
		slog.Error("post: lookup failed", "err", err)
		r.Fail("Could not query the library.")
		return
	}
	if len(records) == 0 {
		r.Fail("No items found for that %s.", ref.kind)
		return
	}

	mentions, roleIDs, missing, err := resolvePings(e.Client(), *e.GuildID(), pings)
	if err != nil {
		slog.Error("post: could not read guild roles", "err", err)
		r.Fail("Could not read this server's roles.")
		return
	}
	if len(missing) > 0 {
		r.Fail("No role here is called %s — pick from the suggestions so the ping actually resolves.",
			quoteList(missing))
		return
	}

	author := ""
	if !anonymous {
		author = e.User().EffectiveName()
	}

	layout := postLayout{
		author:   author,
		pings:    mentions,
		mirror:   mirror,
		source:   source,
		pageLink: ref.pageLink(),
		previews: previewsForPost(records, postPreviewCount),
	}

	r.Publish(discord.NewMessageCreate().
		WithContent(layout.render()).
		WithAllowedMentions(allowedRoleMentions(roleIDs)))
}

// quoteList renders names as `a`, `b` and `c` for an error message.
func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "`" + name + "`"
	}
	if len(quoted) == 1 {
		return quoted[0]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}
