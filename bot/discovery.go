package bot

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// /random and /top: discovery commands over the contents library. Both take an
// optional idol and/or group (autocompleted from PocketBase, so nobody has to
// remember the exact spelling) and a time period. With neither name given the
// scope is the whole library.

// periodChoices are the selectable time windows. Values are parsed by
// periodStart; names double as display labels.
var periodChoices = []discord.ApplicationCommandOptionChoiceString{
	{Name: "Week", Value: "week"},
	{Name: "2 weeks", Value: "2weeks"},
	{Name: "Month", Value: "month"},
	{Name: "Half year", Value: "halfyear"},
	{Name: "Year", Value: "year"},
	{Name: "All time", Value: "all"},
}

// defaultPeriod is the window used when the option is omitted.
const defaultPeriod = "week"

// maxDiscoveryResults caps the `count` option on /random and /top. Five keeps
// the message inside a readable length once every pick embeds its media.
const maxDiscoveryResults = 5

// maxDiscoveryRecords bounds how much of a scope is examined. /top ranks by
// like count, which lives in a relation array and so has to be sorted in Go
// rather than in the query — a scope with more matches than this is ranked
// over its newest maxDiscoveryRecords items only.
const maxDiscoveryRecords = 2000

// periodStart returns the cutoff for a period value; ok=false means no cutoff
// (all time).
func periodStart(period string) (time.Time, bool) {
	now := time.Now().UTC()
	switch period {
	case "week":
		return now.AddDate(0, 0, -7), true
	case "2weeks":
		return now.AddDate(0, 0, -14), true
	case "month":
		return now.AddDate(0, -1, 0), true
	case "halfyear":
		return now.AddDate(0, -6, 0), true
	case "year":
		return now.AddDate(-1, 0, 0), true
	}
	return time.Time{}, false
}

func periodLabel(period string) string {
	for _, c := range periodChoices {
		if c.Value == period {
			return c.Name
		}
	}
	return period
}

func discoveryOptions() []discord.ApplicationCommandOption {
	return []discord.ApplicationCommandOption{
		discord.ApplicationCommandOptionString{
			Name:         "idol",
			Description:  "Idol to scope to (leave empty for any)",
			Autocomplete: true,
		},
		discord.ApplicationCommandOptionString{
			Name:         "group",
			Description:  "Group to scope to (leave empty for any)",
			Autocomplete: true,
		},
		discord.ApplicationCommandOptionString{
			Name:        "period",
			Description: "Time window (default: week)",
			Choices:     periodChoices,
		},
		discord.ApplicationCommandOptionInt{
			Name:        "count",
			Description: fmt.Sprintf("How many results to return (1‑%d, default 1)", maxDiscoveryResults),
			MinValue:    ptr(1),
			MaxValue:    ptr(maxDiscoveryResults),
		},
		discord.ApplicationCommandOptionString{
			Name:        "format",
			Description: "How to post it (default: embed — links the content page)",
			Choices:     discoveryFormatChoices,
		},
	}
}

// Link formats. A content record carries several derivatives of the same item,
// so the caller picks what to hand out:
//
//	embed   — the goyangi content page, which Discord unfurls into a rich
//	          embed. One rendering per item, and no raw URL on show.
//	preview — the animated AVIF/WebP file (equal to the MP4 for `video` records)
//	mp4     — the canonical AV1 MP4 file in R2
//	static  — the poster frame
//
// The file formats hand back a direct media URL. Discord keeps that URL
// visible as text *and* renders the file under it, which is the point when
// somebody asks for the file itself — but it's why `embed` is the default.
var (
	discoveryFormatChoices = []discord.ApplicationCommandOptionChoiceString{
		{Name: "Embed (link the content page)", Value: formatEmbed},
		{Name: "Preview (animated AVIF/WebP)", Value: formatPreview},
		{Name: "MP4 (AV1)", Value: formatMP4},
	}
	unwrapFormatChoices = []discord.ApplicationCommandOptionChoiceString{
		{Name: "MP4 (AV1)", Value: formatMP4},
		{Name: "Preview (animated AVIF/WebP)", Value: formatPreview},
		{Name: "Static thumbnail", Value: formatStatic},
	}
)

const (
	formatEmbed   = "embed"
	formatMP4     = "mp4"
	formatSD      = "sd"
	formatPreview = "preview"
	formatStatic  = "static"
)

// formatFallback is the record-field precedence for each *file* format, so an
// item missing the exact derivative still yields something playable. The
// `mirror` (imgur) copy is last everywhere — it is a third-party host we don't
// control, and it's what /revive and /match exist to work around.
//
// formatEmbed has no entry: it resolves to the content page rather than to a
// stored file, and is handled by the renderer.
func formatFallback(format string) []string {
	switch format {
	case formatMP4:
		return []string{"original", "preview", "mirror"}
	case formatSD:
		// `sd` is best-effort and absent on records predating the rendition, so
		// this falls through to the AV1 original rather than to nothing — a
		// viewer asking for the compatibility copy still gets something.
		return []string{"sd", "original", "preview", "mirror"}
	case formatStatic:
		return []string{"static", "preview", "original", "mirror"}
	default:
		return []string{"preview", "original", "mirror"}
	}
}

// resolveLink walks the precedence for a format, asking get for each field in
// turn. Split out from mediaLink so the table is testable without a database.
func resolveLink(format string, get func(field string) string) string {
	for _, field := range formatFallback(format) {
		if v := get(field); v != "" {
			return v
		}
	}
	return ""
}

// mediaLink resolves a record to a single public URL for the requested format,
// falling back to the PocketBase-hosted file when no R2 derivative exists yet
// (a record whose upload hook hasn't finished).
func mediaLink(record *core.Record, format string) string {
	if link := resolveLink(format, record.GetString); link != "" {
		return link
	}
	if file := record.GetString("file"); file != "" {
		return buildFileLink(record.Id, file)
	}
	return ""
}

func likeCount(record *core.Record) int {
	return len(record.GetStringSlice("likes"))
}

// contentPageLink is the goyangi single-content page for a record.
func contentPageLink(recordID string) string {
	return fmt.Sprintf("%s/single/%s", publicBaseURL, recordID)
}

// ---------------------------------------------------------------------------
// Idol / group directory
// ---------------------------------------------------------------------------

// The directory backs both autocomplete — which fires on every keystroke and
// must answer within 3s — and the /random and /top filters. Both tables are
// small and change rarely (idols and groups are added by hand), so a short TTL
// cache keeps a burst of keystrokes from re-reading them each time.
const directoryTTL = time.Minute

// aliases are the extra strings a record answers to, from its `aliases` field.
// Never shown in a label — they exist for matching what people actually type.
type groupEntry struct {
	id      string
	name    string
	aliases []string
}

type idolEntry struct {
	id        string
	name      string
	aliases   []string
	groupID   string
	groupName string
}

type directory struct {
	groups []groupEntry
	idols  []idolEntry
	// tagNames are the curated system tags, for /reupload's tag picker. Names,
	// not ids: relation resolution looks tags up by name (see resolveRelations).
	tagNames []string
}

var (
	directoryMu     sync.Mutex
	directoryCache  *directory
	directoryLoaded time.Time
)

func loadDirectory() (*directory, error) {
	directoryMu.Lock()
	defer directoryMu.Unlock()

	if directoryCache != nil && time.Since(directoryLoaded) < directoryTTL {
		return directoryCache, nil
	}

	groupRecords, err := App.FindRecordsByFilter("groups", "", "name", 0, 0)
	if err != nil {
		slog.Error("discovery: could not read groups", "err", err)
		return nil, fmt.Errorf("could not read the group list")
	}
	idolRecords, err := App.FindRecordsByFilter("groups_idols", "", "name", 0, 0)
	if err != nil {
		slog.Error("discovery: could not read idols", "err", err)
		return nil, fmt.Errorf("could not read the idol list")
	}

	// Tags are a small curated list and a missing one must not break the idol
	// and group pickers, so a read failure here is logged and left empty rather
	// than failing the whole directory load.
	tagRecords, err := App.FindRecordsByFilter("tags", "", "name", 0, 0)
	if err != nil {
		slog.Warn("discovery: could not read tags", "err", err)
	}

	dir := &directory{
		groups:   make([]groupEntry, 0, len(groupRecords)),
		idols:    make([]idolEntry, 0, len(idolRecords)),
		tagNames: make([]string, 0, len(tagRecords)),
	}
	for _, r := range tagRecords {
		name := strings.TrimSpace(r.GetString("name"))
		// A comma would break the multi-pick option value, which is comma
		// separated. None exist today; this keeps one from being introduced.
		if name == "" || strings.Contains(name, ",") {
			continue
		}
		dir.tagNames = append(dir.tagNames, name)
	}
	groupNames := make(map[string]string, len(groupRecords))
	for _, r := range groupRecords {
		name := strings.TrimSpace(r.GetString("name"))
		groupNames[r.Id] = name
		dir.groups = append(dir.groups, groupEntry{
			id:      r.Id,
			name:    name,
			aliases: splitTrim(r.GetString("aliases")),
		})
	}
	for _, r := range idolRecords {
		groupID := r.GetString("group")
		dir.idols = append(dir.idols, idolEntry{
			id:        r.Id,
			name:      strings.TrimSpace(r.GetString("name")),
			aliases:   splitTrim(r.GetString("aliases")),
			groupID:   groupID,
			groupName: groupNames[groupID],
		})
	}

	directoryCache, directoryLoaded = dir, time.Now()
	return dir, nil
}

// findGroup resolves a group option value. Autocomplete supplies a record id;
// anything typed by hand is matched on name, case-insensitively.
func findGroup(dir *directory, value string) (groupEntry, bool) {
	needle := strings.ToLower(strings.TrimSpace(value))
	if needle == "" {
		return groupEntry{}, false
	}
	for _, g := range dir.groups {
		if g.id == value {
			return g, true
		}
	}
	for _, g := range dir.groups {
		if matchesAnyName(needle, g.name, g.aliases) {
			return g, true
		}
	}
	return groupEntry{}, false
}

// matchesAnyName reports whether an already-lowercased needle equals a record's
// canonical name or any of its aliases.
func matchesAnyName(needle, name string, aliases []string) bool {
	if strings.ToLower(strings.TrimSpace(name)) == needle {
		return true
	}
	for _, alias := range aliases {
		if strings.ToLower(strings.TrimSpace(alias)) == needle {
			return true
		}
	}
	return false
}

// findIdols resolves an idol option value, optionally restricted to a group.
// A hand-typed name can legitimately match several idols across groups (the
// reason `group` used to be mandatory) — all of them are returned so the
// caller can match on any.
func findIdols(dir *directory, value, groupID string) []idolEntry {
	needle := strings.ToLower(strings.TrimSpace(value))
	if needle == "" {
		return nil
	}
	for _, idol := range dir.idols {
		if idol.id == value && (groupID == "" || idol.groupID == groupID) {
			return []idolEntry{idol}
		}
	}
	var matches []idolEntry
	for _, idol := range dir.idols {
		if !matchesAnyName(needle, idol.name, idol.aliases) {
			continue
		}
		if groupID != "" && idol.groupID != groupID {
			continue
		}
		matches = append(matches, idol)
	}
	return matches
}

// ---------------------------------------------------------------------------
// Autocomplete
// ---------------------------------------------------------------------------

// maxAutocompleteChoices is Discord's per-response cap on suggestions.
const maxAutocompleteChoices = 25

// onAutocomplete serves the idol and group suggestions for /random and /top,
// and the role suggestions for /post. For idol and group the choice values are
// record ids, so a picked suggestion resolves exactly regardless of spelling or
// duplicate names.
func onAutocomplete(e *events.AutocompleteInteractionCreate) {
	defer recoverHandler("autocomplete")

	focused := e.Data.Focused()

	// /post's role pings live in post.go — they resolve against the guild, not
	// the library.
	if e.Data.CommandName == "post" && focused.Name == "pings" {
		if e.GuildID() == nil {
			respondAutocomplete(e, focused.Name, nil)
			return
		}
		roles, err := loadGuildRoles(e.Client(), *e.GuildID())
		if err != nil {
			slog.Warn("autocomplete: could not read guild roles", "err", err)
			respondAutocomplete(e, focused.Name, nil)
			return
		}
		respondAutocomplete(e, focused.Name, pingChoices(roles, focused.String()))
		return
	}

	// /reupload's own options accumulate several picks into one value, so they
	// have their own builders rather than the single-value ones below.
	if e.Data.CommandName == "reupload" && (focused.Name == "idols" || focused.Name == "tags") {
		dir, err := loadDirectory()
		if err != nil {
			respondAutocomplete(e, focused.Name, nil)
			return
		}
		respondAutocomplete(e, focused.Name, multiPickChoices(dir, focused.Name, focused.String()))
		return
	}

	switch e.Data.CommandName {
	case "random", "top":
	default:
		return
	}

	if focused.Name != "idol" && focused.Name != "group" {
		return
	}

	dir, err := loadDirectory()
	if err != nil {
		// An empty list is the only way to say "no suggestions" — Discord
		// shows the user's raw input, which still resolves by name.
		respondAutocomplete(e, focused.Name, nil)
		return
	}

	needle := strings.ToLower(strings.TrimSpace(focused.String()))
	choices := make([]discord.AutocompleteChoice, 0, maxAutocompleteChoices)

	if focused.Name == "group" {
		for _, g := range dir.groups {
			if len(choices) == maxAutocompleteChoices {
				break
			}
			if !matchesNeedle(g.name, needle) {
				continue
			}
			choices = append(choices, discord.AutocompleteChoiceString{Name: g.name, Value: g.id})
		}
		respondAutocomplete(e, focused.Name, choices)
		return
	}

	// Scope idol suggestions to the group already filled in, when there is
	// one, so picking "ITZY" first narrows the list to its members.
	groupID := ""
	if g, ok := findGroup(dir, e.Data.String("group")); ok {
		groupID = g.id
	}
	for _, idol := range dir.idols {
		if len(choices) == maxAutocompleteChoices {
			break
		}
		if groupID != "" && idol.groupID != groupID {
			continue
		}
		if !matchesNeedle(idol.name, needle) {
			continue
		}
		choices = append(choices, discord.AutocompleteChoiceString{
			Name:  idolChoiceLabel(idol),
			Value: idol.id,
		})
	}
	respondAutocomplete(e, focused.Name, choices)
}

func matchesNeedle(name, needle string) bool {
	return needle == "" || strings.Contains(strings.ToLower(name), needle)
}

// idolLabelSeparator joins an idol to its group in a choice's display name.
//
// A named constant because the label is not only rendered — it comes BACK.
// Discord echoes the displayed text, not the choice value, once the user keeps
// typing (see idolFromEntry), so this is a wire format in both directions.
const idolLabelSeparator = " — "

func idolChoiceLabel(idol idolEntry) string {
	if idol.groupName == "" {
		return idol.name
	}
	return idol.name + idolLabelSeparator + idol.groupName
}

func respondAutocomplete(e *events.AutocompleteInteractionCreate, option string, choices []discord.AutocompleteChoice) {
	if err := e.AutocompleteResult(choices); err != nil {
		slog.Error("autocomplete: could not respond", "option", option, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Scope resolution and lookup
// ---------------------------------------------------------------------------

// discoveryScope is the resolved idol/group filter for one invocation.
type discoveryScope struct {
	filter string // "" when unscoped — the whole library
	params dbx.Params
	label  string // "Yuna · ITZY", "ITZY", "Everything"
}

// resolveScope turns the optional idol and group options into a PocketBase
// filter. Names are resolved against the directory rather than interpolated
// into the filter string, so user-supplied text can never reach the query.
func resolveScope(idolOpt, groupOpt string) (discoveryScope, error) {
	scope := discoveryScope{params: dbx.Params{}, label: "Everything"}
	if idolOpt == "" && groupOpt == "" {
		return scope, nil
	}

	dir, err := loadDirectory()
	if err != nil {
		return scope, err
	}

	var conditions, labels []string

	groupID := ""
	if groupOpt != "" {
		group, ok := findGroup(dir, groupOpt)
		if !ok {
			return scope, fmt.Errorf("group %q is not registered in goyangi", groupOpt)
		}
		groupID = group.id
		conditions = append(conditions, "group ~ {:group}")
		scope.params["group"] = group.id
		labels = append(labels, group.name)
	}

	if idolOpt != "" {
		matches := findIdols(dir, idolOpt, groupID)
		if len(matches) == 0 {
			if groupID != "" {
				return scope, fmt.Errorf("idol %q is not registered in %s", idolOpt, labels[0])
			}
			return scope, fmt.Errorf("idol %q is not registered in goyangi", idolOpt)
		}

		ors := make([]string, 0, len(matches))
		for i, idol := range matches {
			key := fmt.Sprintf("idol%d", i)
			ors = append(ors, fmt.Sprintf("idol ~ {:%s}", key))
			scope.params[key] = idol.id
		}
		conditions = append(conditions, "("+strings.Join(ors, " || ")+")")

		label := matches[0].name
		if len(matches) > 1 {
			label += " (all groups)"
		}
		labels = append([]string{label}, labels...)
	}

	scope.filter = strings.Join(conditions, " && ")
	scope.label = strings.Join(labels, " · ")
	return scope, nil
}

// findContents returns the records in scope for the period, newest first. An
// empty scope filter means the whole library.
func findContents(scope discoveryScope, period string) ([]*core.Record, error) {
	filter := scope.filter
	params := dbx.Params{}
	for k, v := range scope.params {
		params[k] = v
	}

	if since, bounded := periodStart(period); bounded {
		if filter != "" {
			filter += " && "
		}
		filter += "created >= {:since}"
		// PocketBase stores DateTime as "2006-01-02 15:04:05.000Z" strings.
		params["since"] = since.Format("2006-01-02 15:04:05.000Z")
	}
	return App.FindRecordsByFilter("contents", filter, "-created", maxDiscoveryRecords, 0, params)
}

// discoveryRequest is one parsed /random or /top invocation.
type discoveryRequest struct {
	scope   discoveryScope
	records []*core.Record
	period  string
	format  string
	count   int
}

// parseDiscovery reads the shared options and runs the lookup, reporting every
// failure privately. ok=false means the caller is done.
func parseDiscovery(r *reply, data discord.SlashCommandInteractionData) (discoveryRequest, bool) {
	req := discoveryRequest{
		period: optString(data, "period", defaultPeriod),
		format: optString(data, "format", formatEmbed),
		count:  min(max(data.Int("count"), 1), maxDiscoveryResults),
	}

	scope, err := resolveScope(data.String("idol"), data.String("group"))
	if err != nil {
		r.Fail("%s", err.Error())
		return req, false
	}
	req.scope = scope

	records, err := findContents(scope, req.period)
	if err != nil {
		slog.Error("discovery: lookup failed", "interaction", r.label, "err", err)
		r.Fail("Could not query the library.")
		return req, false
	}
	if len(records) == 0 {
		r.Fail("Nothing found for **%s** in the last %s.", scope.label, strings.ToLower(periodLabel(req.period)))
		return req, false
	}
	req.records = records
	return req, true
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func handleRandomCommand(e *events.ApplicationCommandInteractionCreate, data discord.SlashCommandInteractionData) {
	r := newReply(e)
	if !r.Defer() {
		return
	}

	req, ok := parseDiscovery(r, data)
	if !ok {
		return
	}

	// Sample without replacement so a multi-item draw can't repeat an item.
	order := rand.Perm(len(req.records))[:min(req.count, len(req.records))]
	picks := make([]*core.Record, 0, len(order))
	for _, idx := range order {
		picks = append(picks, req.records[idx])
	}

	publishDiscovery(r, "🎲 Random", req, picks)
}

func handleTopCommand(e *events.ApplicationCommandInteractionCreate, data discord.SlashCommandInteractionData) {
	r := newReply(e)
	if !r.Defer() {
		return
	}

	req, ok := parseDiscovery(r, data)
	if !ok {
		return
	}

	// Likes live in a relation array on the record, so rank in Go. Stable:
	// ties keep the newest-first order from the query.
	records := req.records
	sort.SliceStable(records, func(i, j int) bool {
		return likeCount(records[i]) > likeCount(records[j])
	})

	publishDiscovery(r, "🏆 Top", req, records[:min(req.count, len(records))])
}

// publishDiscovery renders and posts the result: a header, then one block per
// pick.
//
// In embed mode the content page link goes out bare and Discord unfurls it —
// one rendering per item, title and thumbnail included, no raw URL on show.
// For the file formats the media URL has to be bare for Discord to render it,
// so the title moves into a masked link (which never unfurls) to keep the item
// down to a single embed.
func publishDiscovery(r *reply, heading string, req discoveryRequest, picks []*core.Record) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s · **%s** · %s — %d of %d\n",
		heading, req.scope.label, periodLabel(req.period), len(picks), len(req.records))

	rendered := 0
	for _, record := range picks {
		if req.format == formatEmbed {
			fmt.Fprintf(&sb, "❤️ %d · %s\n", likeCount(record), contentPageLink(record.Id))
			rendered++
			continue
		}

		link := mediaLink(record, req.format)
		if link == "" {
			continue
		}
		fmt.Fprintf(&sb, "❤️ %d · [%s](%s)\n%s\n",
			likeCount(record), discordSafeTitle(record.GetString("title")),
			contentPageLink(record.Id), link)
		rendered++
	}

	if rendered == 0 {
		r.Fail("Found %d match(es) but none has a usable %s link.", len(picks), req.format)
		return
	}

	r.Publish(discord.NewMessageCreate().WithContent(sb.String()))
}

// discordSafeTitle keeps a title from breaking the masked-link syntax it is
// rendered into (a `]` would close the label early).
func discordSafeTitle(title string) string {
	title = strings.NewReplacer("[", "(", "]", ")", "\n", " ").Replace(strings.TrimSpace(title))
	if title == "" {
		return "untitled"
	}
	return title
}
