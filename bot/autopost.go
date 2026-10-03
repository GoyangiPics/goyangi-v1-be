package bot

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/pocketbase/pocketbase/core"
)

// Content uploaded on the site is announced in the post channel once its
// renditions are ready, in the same layout /post produces (post.go).
//
// Items ingested from Discord are skipped: they were already posted by hand in
// the channel the bot took them from, so announcing them would echo the server
// back at itself.

// uploadPostWindow debounces a batch. A multi-file upload finishes encoding one
// file at a time, and each completion pushes the flush back, so the set is
// announced once — after the last file lands — rather than per file.
const uploadPostWindow = 30 * time.Second

// pendingUpload is one thing to announce. Keyed by set so every file in a
// multi-file upload collapses to a single post.
type pendingUpload struct {
	setID    string // "" when the item belongs to no set
	recordID string
}

var (
	uploadPostMu    sync.Mutex
	pendingUploads  = map[string]pendingUpload{}
	uploadPostTimer *time.Timer
)

// QueueUploadPost is called once a content record's renditions are in R2 and
// the record has been saved. Wired to hooks.OnContentPublished from main.go.
func QueueUploadPost(recordID string) {
	if App == nil || Client == nil {
		return // bot isn't up; nothing to post to
	}

	record, err := App.FindRecordById("contents", recordID)
	if err != nil {
		slog.Warn("autopost: could not read record", "record", recordID, "err", err)
		return
	}
	// The "discord" field holds the message an item was ingested from, so a
	// non-empty value means this did not come from the site.
	if record.GetString("discord") != "" {
		return
	}

	setID := record.GetString("set")

	// Collection-mode uploads are not announced.
	//
	// Announcements are keyed by set, so a 20-file set upload collapses to one
	// message. A setless upload has no such key and would post once per file —
	// twenty messages, each carrying idol/group role pings. Collections are
	// personal curation rather than something to broadcast, so they stay silent.
	//
	// Specifically "no set AND at least one collection", not "any setless
	// upload": stickers are also setless, carry no collections, and are announced
	// today.
	if setID == "" && len(record.GetStringSlice("collections")) > 0 {
		slog.Info("autopost: skipping a collection upload", "record", recordID)
		return
	}

	key := "set:" + setID
	if setID == "" {
		key = "single:" + recordID
	}

	uploadPostMu.Lock()
	defer uploadPostMu.Unlock()

	pendingUploads[key] = pendingUpload{setID: setID, recordID: recordID}
	if uploadPostTimer != nil {
		uploadPostTimer.Stop()
	}
	uploadPostTimer = time.AfterFunc(uploadPostWindow, flushUploadPosts)

	slog.Info("autopost: queued upload", "record", recordID, "set", setID, "pending", len(pendingUploads))
}

func flushUploadPosts() {
	flushesInFlight.Add(1)
	defer flushesInFlight.Add(-1)

	uploadPostMu.Lock()
	batch := pendingUploads
	pendingUploads = map[string]pendingUpload{}
	uploadPostTimer = nil
	uploadPostMu.Unlock()

	channelID := uploadPostChannelID()
	if channelID == 0 {
		slog.Warn("autopost: no post channel configured, dropping batch", "count", len(batch))
		return
	}

	for key, pending := range batch {
		if err := postUpload(channelID, pending); err != nil {
			slog.Error("autopost: could not post upload", "key", key, "err", err)
		}
	}
}

// postUpload announces one upload: a whole set when the items belong to one,
// otherwise the single item.
func postUpload(channelID snowflake.ID, pending pendingUpload) error {
	var (
		records  []*core.Record
		pageLink string
		err      error
	)

	if pending.setID != "" {
		ref := setRef{kind: "set", id: pending.setID, filter: "set = {:id}"}
		if records, err = findSetContents(ref); err != nil {
			return err
		}
		pageLink = ref.pageLink()
	} else {
		record, ferr := App.FindRecordById("contents", pending.recordID)
		if ferr != nil {
			return ferr
		}
		records = []*core.Record{record}
		pageLink = contentPageLink(record.Id)
	}

	if len(records) == 0 {
		slog.Warn("autopost: nothing to post", "set", pending.setID, "record", pending.recordID)
		return nil
	}
	lead := records[0]

	mentions, roleIDs := autoPings(lead)

	layout := postLayout{
		author:   uploaderName(lead),
		pings:    mentions,
		mirror:   lead.GetString("mirror"),
		source:   lead.GetString("source"),
		pageLink: pageLink,
		previews: previewsForPost(records, postPreviewCount),
	}

	_, err = Client.Rest.CreateMessage(channelID, discord.NewMessageCreate().
		WithContent(layout.render()).
		WithAllowedMentions(allowedRoleMentions(roleIDs)))
	return err
}

// uploaderName is the byline for an automatic post — the record's uploader, not
// a Discord user. Empty when unset, which renders as no byline at all.
func uploaderName(record *core.Record) string {
	if errs := App.ExpandRecord(record, []string{"uploader"}, nil); len(errs) > 0 {
		slog.Warn("autopost: could not expand uploader", "record", record.Id)
		return ""
	}
	for _, uploader := range record.ExpandedAll("uploader") {
		if name := strings.TrimSpace(uploader.GetString("name")); name != "" {
			return name
		}
	}
	return ""
}

// autoPings resolves the record's idols and groups to guild roles, matching the
// same "Idol [Group]" naming that role-ping ingestion parses, plus a bare role
// per group. Names with no matching role are skipped silently — a missing role
// is a server-setup gap, not something worth failing an upload announcement
// over.
func autoPings(record *core.Record) (mentions []string, roleIDs []snowflake.ID) {
	guildID := postGuildID()
	if guildID == 0 {
		return nil, nil
	}

	roles, err := loadGuildRoles(Client, guildID)
	if err != nil {
		slog.Warn("autopost: could not read guild roles", "err", err)
		return nil, nil
	}

	dir, err := loadDirectory()
	if err != nil {
		slog.Warn("autopost: could not read the idol directory", "err", err)
		return nil, nil
	}

	// Idol roles are the specific ping, group roles the catch-all, and the
	// mentions read in that order. Each idol contributes at most one role:
	// the qualified "Idol [Group]" name if that role exists, else the bare
	// name — pinging both would notify the same people twice.
	var wanted [][]string
	for _, idolID := range record.GetStringSlice("idol") {
		for _, idol := range findIdols(dir, idolID, "") {
			preference := []string{idol.name}
			if idol.groupName != "" {
				preference = []string{idol.name + " [" + idol.groupName + "]", idol.name}
			}
			wanted = append(wanted, preference)
		}
	}
	for _, groupID := range record.GetStringSlice("group") {
		for _, group := range dir.groups {
			if group.id == groupID {
				wanted = append(wanted, []string{group.name})
			}
		}
	}

	seen := make(map[snowflake.ID]bool, len(wanted))
	for _, preference := range wanted {
		for _, name := range preference {
			role, ok := findRoleByName(roles, name)
			if !ok {
				continue
			}
			if !seen[role.ID] {
				seen[role.ID] = true
				roleIDs = append(roleIDs, role.ID)
				mentions = append(mentions, discord.RoleMention(role.ID))
			}
			break // first match wins for this idol/group
		}
	}
	return mentions, roleIDs
}
