package bot

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
)

// reuploadCommandCreate defines /reupload.
//
// The gap it fills: the "Ingest this message" context menu credits the message
// author and reads only that message. This takes an explicit idol and tag list
// and lands on the site credited to whoever ran the command.
//
// DefaultMemberPermissions hides it from regular members. That is a hint some
// clients ignore, so the role check is the enforced gate; see
// manualIngestRoleIDs.
func reuploadCommandCreate() discord.SlashCommandCreate {
	return discord.SlashCommandCreate{
		Name:        "reupload",
		Description: "Ingest the media in a Discord message into the library, credited to you.",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "link",
				Description: "Link to the message (right-click the message → Copy Message Link)",
				Required:    true,
			},
			discord.ApplicationCommandOptionString{
				// One picker rather than an idol and a group: every idol in the
				// directory already carries its group, so choosing the idol
				// determines the pair. Pick repeatedly to build a list — the
				// autocomplete appends to what is already there.
				Name:         "idols",
				Description:  "Idol — pick again to add another",
				Required:     true,
				Autocomplete: true,
			},
			discord.ApplicationCommandOptionString{
				Name:         "tags",
				Description:  "Tags — pick again to add another",
				Autocomplete: true,
			},
			discord.ApplicationCommandOptionString{
				// Optional: drop the items into an existing set instead of the
				// new one /reupload creates by default. A set link like
				// https://goyangi.pics/set/<id>; collection links are rejected.
				Name:        "set",
				Description: "Optional: add to an existing set — paste a set link (…/set/<id>)",
			},
		},
		DefaultMemberPermissions: omit.NewPtr(discord.PermissionManageMessages),
	}
}

// Discord caps an autocomplete choice's name and value at 100 characters each.
// For the accumulating options that is also the ceiling on how many picks fit in
// one value: idol ids are 15 characters plus a separator, so roughly six.
const autocompleteValueLimit = 100

// multiPickState splits an accumulating option's raw value into the entries
// already chosen and the fragment still being typed.
//
// Every value this hands back ends with a comma, which is what makes the split
// unambiguous: a trailing comma means the last entry is a completed pick, its
// absence means the user is mid-word. Discord replaces the entire option value
// when a suggestion is chosen, so there is nothing else to track.
func multiPickState(raw string) (chosen []string, needle string) {
	trimmed := strings.TrimSpace(raw)
	parts := splitTrim(trimmed)
	if len(parts) == 0 {
		return nil, ""
	}
	if strings.HasSuffix(trimmed, ",") {
		return parts, ""
	}
	return parts[:len(parts)-1], strings.ToLower(parts[len(parts)-1])
}

// multiPickChoices builds the suggestions for /reupload's `idols` and `tags`.
//
// Each suggestion's value is everything already picked plus this entry, so
// choosing one appends rather than replaces. The label spells the whole
// selection out, since the raw value is a list of record ids for idols and
// therefore unreadable in the input box.
func multiPickChoices(dir *directory, option, raw string) []discord.AutocompleteChoice {
	chosen, needle := multiPickState(raw)

	// Every already-chosen entry is resolved back to its id before the next
	// value is built. Without this the option accumulated whatever Discord
	// happened to echo, which for a second pick is the previous choice's LABEL —
	// producing "Sui — KiiiKiii,<id>," and an "I don't know" on submit.
	//
	// Rebuilding from ids makes the value self-healing at any depth: however
	// many labels come back in, all ids go out.
	canonical := make([]string, 0, len(chosen))
	prefixLabels := make([]string, 0, len(chosen))
	picked := make(map[string]bool, len(chosen))
	for _, entry := range chosen {
		value, label := canonicalPick(dir, option, entry)
		// Keyed on the resolved id, so an idol already picked under its label
		// isn't offered a second time.
		picked[value] = true
		canonical = append(canonical, value)
		prefixLabels = append(prefixLabels, label)
	}

	choices := make([]discord.AutocompleteChoice, 0, maxAutocompleteChoices)
	add := func(entry, label string) bool {
		if picked[entry] {
			return true
		}
		value := strings.Join(append(append([]string{}, canonical...), entry), ",") + ","
		// Silently skipping an entry that doesn't fit beats offering a
		// suggestion Discord will reject.
		if len(value) > autocompleteValueLimit {
			return true
		}
		name := strings.Join(append(append([]string{}, prefixLabels...), label), ", ")
		if len(name) > autocompleteValueLimit {
			name = name[:autocompleteValueLimit-1] + "…"
		}
		choices = append(choices, discord.AutocompleteChoiceString{Name: name, Value: value})
		return len(choices) < maxAutocompleteChoices
	}

	if option == "tags" {
		for _, tag := range dir.tagNames {
			if !matchesNeedle(tag, needle) {
				continue
			}
			if !add(tag, tag) {
				break
			}
		}
		return choices
	}

	for _, idol := range dir.idols {
		if !matchesNeedle(idol.name, needle) {
			continue
		}
		if !add(idol.id, idolChoiceLabel(idol)) {
			break
		}
	}
	return choices
}

// canonicalPick resolves one already-chosen entry to the value that should be
// re-emitted and the label that should be displayed for it.
//
// Tags need no resolution: their choice value IS the tag name, so what comes
// back is already canonical. Idols are the case that matters — value and label
// differ, and only the label survives a round trip through the input box.
// Anything unresolvable is passed through untouched so a hand-typed name keeps
// working and reaches resolveReuploadIdols to be reported by name.
func canonicalPick(dir *directory, option, entry string) (value, label string) {
	if option == "tags" {
		return entry, entry
	}
	if idol, ok := idolFromEntry(dir, entry); ok {
		return idol.id, idolChoiceLabel(idol)
	}
	return entry, entry
}

// idolFromEntry resolves one entry of an `idols` option value.
//
// Three forms reach here, and they have to be told apart:
//
//   - a record id, which is what the autocomplete emits;
//   - "Name — Group", the choice LABEL, which Discord echoes back into the
//     option once the user types a comma to add another idol — the input box
//     keeps the displayed text, not the value behind it, so this is what a
//     second pick is actually built from;
//   - a bare hand-typed name, which has always been supported.
//
// The group half of a label is used to disambiguate a name shared across
// groups, but is not required to match: a group renamed since the label was
// rendered should not turn a valid pick into "I don't know".
func idolFromEntry(dir *directory, entry string) (idolEntry, bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return idolEntry{}, false
	}

	// Covers the id and the bare-name forms.
	if matches := findIdols(dir, entry, ""); len(matches) > 0 {
		return matches[0], true
	}

	name, group, found := strings.Cut(entry, idolLabelSeparator)
	if !found {
		return idolEntry{}, false
	}
	name, group = strings.TrimSpace(name), strings.TrimSpace(group)
	matches := findIdols(dir, name, "")
	if len(matches) == 0 {
		return idolEntry{}, false
	}
	for _, idol := range matches {
		if strings.EqualFold(idol.groupName, group) {
			return idol, true
		}
	}
	return matches[0], true
}

// resolveReuploadIdols turns the picked `idols` value into the metadata pair.
//
// Idols carry their group, so the groups are whatever the chosen idols belong
// to, de-duplicated and order-preserved. Hand-typed names are still accepted, so
// somebody who types "Yujin" rather than picking gets the same result.
func resolveReuploadIdols(dir *directory, raw string) (idolNames, groupNames []string, unknown []string) {
	seenIdol := map[string]bool{}
	seenGroup := map[string]bool{}

	for _, entry := range splitTrim(raw) {
		// idolFromEntry rather than findIdols: a value submitted without a
		// further pick still carries the label form for everything but the last
		// entry, so this is the second half of the same fix.
		idol, ok := idolFromEntry(dir, entry)
		if !ok {
			unknown = append(unknown, entry)
			continue
		}
		if !seenIdol[idol.id] {
			seenIdol[idol.id] = true
			idolNames = append(idolNames, idol.name)
		}
		if idol.groupName != "" && !seenGroup[idol.groupName] {
			seenGroup[idol.groupName] = true
			groupNames = append(groupNames, idol.groupName)
		}
	}
	return idolNames, groupNames, unknown
}

// callerMayIngestAnywhere reports whether the invoking member holds a role
// allowed to run the ingestion paths that ignore the channel allowlist —
// /reupload and the "Ingest this message" context menu.
//
// Takes the member rather than the event: the context menu's modal submit is a
// separate interaction of a different type, and it has to be re-checked there
// (a custom id is client-supplied, so nothing about the first check carries).
func callerMayIngestAnywhere(member *discord.ResolvedMember) bool {
	// Not configured → fall back to the command's permission hint.
	if len(manualIngestRoleIDs) == 0 {
		return true
	}
	if member == nil {
		return false
	}
	for _, roleID := range member.RoleIDs {
		if manualIngestRoleIDs[roleID] {
			return true
		}
	}
	return false
}

// handleReuploadCommand ingests a linked message as the invoking user.
//
// Follows handleIngestContext step for step, with two deliberate differences:
// no channel-allowlist check, and the uploader is the caller rather than the
// message author.
func handleReuploadCommand(e *events.ApplicationCommandInteractionCreate, data discord.SlashCommandInteractionData) {
	if e.GuildID() == nil {
		respondEphemeral(e, "Reuploading only works in a server.")
		return
	}
	if !callerMayIngestAnywhere(e.Member()) {
		respondEphemeral(e, "You don't have permission to use this command.")
		return
	}
	// The caller is credited as the uploader and this publishes to the site, so
	// their Discord account has to be linked to one that may upload. Checked on
	// both manual paths — a gate only the context menu enforced would be no gate
	// at all, since anyone refused there could run this instead.
	denial, uploaderName := callerUploadDenial(e.User().EffectiveName(), e.User().Username)
	if denial != "" {
		respondEphemeral(e, string(denial))
		return
	}

	ref, err := parseMessageLink(data.String("link"))
	if err != nil {
		respondEphemeral(e, "❌ "+err.Error())
		return
	}

	// Cross-guild would fail at the fetch anyway (the bot may not be in that
	// server); refusing up front gives a clear reason instead of a REST error.
	if ref.guildID != *e.GuildID() {
		respondEphemeral(e, "That message is from a different server.")
		return
	}

	dir, err := loadDirectory()
	if err != nil {
		respondEphemeral(e, "Could not load the idol/group directory. Try again shortly.")
		return
	}
	idolNames, groupNames, unknown := resolveReuploadIdols(dir, data.String("idols"))
	if len(unknown) > 0 {
		respondEphemeral(e, fmt.Sprintf("I don't know: %s.", strings.Join(unknown, ", ")))
		return
	}
	if len(idolNames) == 0 {
		respondEphemeral(e, "Pick at least one idol.")
		return
	}

	runManualIngest(newReply(e), manualIngest{
		channelID:  ref.channelID,
		messageID:  ref.messageID,
		guildID:    *e.GuildID(),
		idolNames:  idolNames,
		groupNames: groupNames,
		tagsRaw:    data.String("tags"),
		setRaw:     data.String("set"),
		// The gate's canonical name, NOT EffectiveName: crediting a name the
		// gate didn't match would mint a stray uploader — see callerUploadDenial.
		uploader: uploaderName,
	})
}

// manualIngest is one request to ingest a named message, however it was asked
// for — /reupload's options or the context menu's modal.
type manualIngest struct {
	channelID snowflake.ID
	messageID snowflake.ID
	guildID   snowflake.ID
	// Already resolved, so the caller owns reporting unknown names in whatever
	// shape suits it.
	idolNames  []string
	groupNames []string
	tagsRaw    string
	setRaw     string
	// Who gets credited — the caller, not the message author.
	uploader string
}

// runManualIngest is everything the two manual paths do once their input is
// resolved: validate an optional set link, fetch the message, collect its media,
// build the metadata and run the pipeline.
//
// Shared because the context menu is, as of the modal, exactly /reupload with a
// different way of asking the same three questions. It was already a near-copy
// of this before that; keeping two would mean the date rule, the claim, the
// uploader credit and the reaction all had to be fixed twice.
func runManualIngest(r *reply, req manualIngest) {
	client := r.e.Client()

	// Optional: add these items to an existing set instead of creating a new
	// one. Validate the link now — before the slow defer/download — so a bad or
	// unknown set fails fast with a clear reason. The id is applied to the
	// metadata below; a non-empty joinSetID also tells runIngestion to skip
	// new-set creation and route the items into this set instead.
	var joinSetID string
	if setLink := strings.TrimSpace(req.setRaw); setLink != "" {
		sref, perr := parseSetLink(setLink)
		if perr != nil {
			// Fail prefixes the ❌ itself.
			r.Fail("%s", perr.Error())
			return
		}
		if sref.kind != "set" {
			r.Fail("%s", "That looks like a collection link — paste a set link (…/set/<id>).")
			return
		}
		// Confirm the set exists so the contents' `set` relation can't dangle.
		if _, ferr := App.FindRecordById("contents_sets", sref.id); ferr != nil {
			r.Fail("%s", "I couldn't find that set — double-check the link.")
			return
		}
		joinSetID = sref.id
	}

	// Download + transcode is slow, so acknowledge before doing any of it.
	if !r.Defer() {
		return
	}

	// The invoking guild is the boundary, and it has to be enforced against the
	// RESOLVED channel, not against anything the request carried. Both paths
	// name their target in client-editable text: the modal's custom id rides
	// through the client, and /reupload's guild check reads the guild SEGMENT of
	// a pasted link — but GetMessage ignores that segment entirely, so a link
	// whose channel id belongs to another guild sailed past it. Without this, an
	// ingest-role holder could pull from any channel the bot can read in any
	// guild it is in.
	ch, cherr := client.Rest.GetChannel(req.channelID)
	if cherr != nil {
		slog.Warn("manual ingest: could not resolve channel", "channel", req.channelID, "err", cherr)
		r.Fail("I can't read that channel — check I have access to it.")
		return
	}
	if guildCh, ok := ch.(discord.GuildChannel); !ok || guildCh.GuildID() != req.guildID {
		r.Fail("That message is from a different server.")
		return
	}

	// A REST fetch is NOT gated by the MESSAGE_CONTENT intent — intents only
	// affect gateway events — so content and attachments come back in full, and
	// attachment URLs come back freshly signed.
	msg, err := client.Rest.GetMessage(req.channelID, req.messageID)
	if err != nil {
		slog.Warn("reupload: could not fetch message", "channel", req.channelID, "message", req.messageID, "err", err)
		r.Fail("I can't read that message — check I have access to that channel.")
		return
	}

	// Other bots' posts are ingestible; only our own output is not.
	if msg.Author.ID == client.ApplicationID {
		r.Fail("The bot's own messages can't be ingested.")
		return
	}

	items := collectMedia(*msg)
	if len(items) == 0 {
		r.Fail("No ingestible media found in that message (attachments or supported links).")
		return
	}

	idolList := strings.Join(req.idolNames, ", ")
	groupList := strings.Join(req.groupNames, ", ")

	// Seeded BEFORE extractMetadata for two reasons: it stops the
	// required-idol/group check inside from failing, and it makes the derived
	// title sensible. Re-asserted after, because a message with its own
	// `idol:`/`group:` lines would otherwise override the explicit options — and
	// the options are what the caller actually asked for.
	metadata := Metadata{
		Idol:     idolList,
		Group:    groupList,
		Uploader: req.uploader,
	}
	if err := extractMetadata(msg.Content, &metadata); err != nil {
		r.Fail("%s", err.Error())
		return
	}
	metadata.Idol, metadata.Group = idolList, groupList

	// Tags from the command win over any `tags:` line, for the same reason the
	// idols do: they are what the caller explicitly asked for.
	if picked := splitTrim(req.tagsRaw); len(picked) > 0 {
		metadata.Tags = strings.Join(picked, ", ")
	}

	// The content is as old as the message it came from, not as old as the
	// reupload. YYMMDD is the form normalizeDate parses and the one
	// createSetRecord prefixes set titles with, so this drives both. A `date:`
	// line in the message still wins — that is someone stating the real date.
	if metadata.Date == "" {
		metadata.Date = msg.CreatedAt.UTC().Format("060102")
	}

	if !claimMessage(req.messageID.String()) {
		r.Fail("That message was already ingested recently.")
		return
	}

	// fillMessageDefaults only fills Uploader when empty, so the caller set above
	// survives and the message author is NOT credited. It also stamps
	// metadata.Discord, which makes the origin hook mark these records as
	// discord-sourced — correct, the media did come from Discord.
	fillMessageDefaults(&metadata, *msg, req.guildID)

	// An explicit set link routes the items into that set; a non-empty
	// joinSetID also makes runIngestion skip creating a new set (it treats a
	// pre-set SetId the same way it treats a reply that joins an existing set).
	if joinSetID != "" {
		metadata.SetId = joinSetID
	}

	outcome := runIngestion(metadata, items, joinSetID != "")

	// React on the source message so the channel it came from shows it was
	// processed, the same signal the context menu leaves.
	reactTo(client, msg.ChannelID, msg.ID, outcome.emoji())
	logOutcome(outcome, metadata.Discord)

	// Ephemeral: the effect is on the site and on the source message, so a
	// channel post here would be noise in whatever channel this was run in.
	r.Private("%s", outcome.text())
}
