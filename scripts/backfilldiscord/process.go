package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"goyangi-v1-be/bot"
	"goyangi-v1-be/hooks"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/pocketbase/pocketbase/tools/filesystem"
)

// One pinged message (plus its same-author replies) becomes one set, exactly as
// the live bot's role-ping trigger followed by its reply trigger would have made
// it. The decisions are the bot's; only the plumbing differs.

type runner struct {
	pb  *pbClient
	dc  *discordREST
	dir *directory
	fs  *filesystem.System // nil in a dry run

	publicURL  string
	commit     bool
	probe      bool
	previewFmt string
	workdir    string
	followups  bool
	replies    bool          // collect same-author replies (-replies)
	origin     string        // what the records are stamped with: script or discord
	sem        chan struct{} // bounds concurrent item encodes

	// chains replays state.go's per-author chain: the set an author's most
	// recent ping opened, so a re-ping about the same idols within the window
	// joins it instead of opening another. Hits arrive oldest first, which is
	// what makes a single pass over them equivalent to the live bot's clock.
	chains map[snowflake.ID]*chain

	// consumed is every message already folded into a set as a reply or a bare
	// follow-up. A scan visits every message with media, so without this the
	// same message would come round again as a candidate of its own.
	consumed map[snowflake.ID]bool

	verified sync.Once
}

// chain is one author's open set, as the live bot would have remembered it.
type chain struct {
	subject  string    // idol and group ids, canonical order
	rootLink string    // the ping that opened it, for the log
	setID    string    // "" in a dry run, or when nothing landed
	lastAt   time.Time // last message that joined
	attached int       // follow-ups and re-pings absorbed so far
}

// setPlan is everything decided about a set before anything is written.
type setPlan struct {
	hit      discord.Message
	meta     bot.Metadata
	rel      resolved
	idolName []string // canonical, relation order
	grpName  []string
	uploader string // credited name
	upID     string
	tagIDs   []string
	items    []plannedItem
}

type plannedItem struct {
	msg  discord.Message
	item bot.MediaItem
}

func (r *runner) processHit(ctx context.Context, hit discord.Message, replies []discord.Message, st *stats) {
	link := jumpLink(guildID, hit)
	skip := func(why string) {
		st.skipped++
		log.Printf("   ⏭  skip: %s", why)
	}

	if hit.Author.ID == r.dc.appID {
		skip("the bot's own message")
		return
	}
	// Role ping + bot mention together is the "already on the site by hand"
	// marker (see bot.prepareIngestion).
	if bot.BotMentionedIn(hit.Content, r.dc.appID.String()) && len(hit.MentionRoles) > 0 {
		skip("marked as already uploaded (role ping + bot mention)")
		return
	}

	// ── Metadata, the role-ping way ──
	var roleNames []string
	for _, id := range hit.MentionRoles {
		if n, ok := r.dc.roles[id]; ok {
			roleNames = append(roleNames, n)
		}
	}
	meta := bot.RolesToMetadata(roleNames)
	detected := false
	if err := bot.ExtractMetadata(hit.Content, &meta); err != nil {
		// Same fall-through as bot.prepareIngestion: a role ping whose roles name
		// no idol (a group-only role), or a message with no ping at all, goes to
		// text detection. An @-mention of the bot is the exception — it states
		// its own metadata and gets no guess.
		if bot.BotMentionedIn(hit.Content, r.dc.appID.String()) {
			skip("@-mention of the bot without usable idol/group lines")
			return
		}
		if len(bot.CollectMedia(hit)) == 0 {
			skip("no ingestible media")
			return
		}
		m, ok := r.detectFromText(hit, roleNames)
		if !ok {
			if len(roleNames) > 0 {
				skip(fmt.Sprintf("no usable idol/group in the pinged roles %v, and the text names none", roleNames))
			} else {
				skip("no ping, and the text names no idol")
			}
			return
		}
		meta, detected = m, true
		// A pinged hit's replies were looked up with the whole batch; a message
		// found by its text was not in it, so its replies are fetched now.
		if r.replies && len(replies) == 0 {
			rm, err := r.dc.repliesTo(ctx, []discord.Message{hit})
			if err != nil {
				st.failed++
				log.Printf("   ❌ reply lookup: %v", err)
				return
			}
			replies = rm[hit.ID]
		}
	}
	rel := r.dir.resolve(bot.SplitTrim(meta.Idol), bot.SplitTrim(meta.Group))
	if len(rel.idolIDs) == 0 || len(rel.groupIDs) == 0 {
		skip(fmt.Sprintf("no matching idol/group in the directory (unresolved: %s)", strings.Join(rel.missing, ", ")))
		return
	}
	idolNames := r.dir.names(r.dir.idolName, rel.idolIDs)
	groupNames := r.dir.names(r.dir.groupName, rel.groupIDs)
	// Retitle a generated title from what actually resolved (bot.runIngestion).
	if meta.Title == bot.AutoTitle(meta.Idol, meta.Group) {
		meta.Title = bot.AutoTitle(strings.Join(idolNames, ", "), strings.Join(groupNames, ", "))
	}

	// ── Same author, same subject, shortly after their last ping: a re-ping
	// that continues the drop rather than a new one (bot.prepareIngestion's
	// sameSubject case). Spends a follow-up slot like any other continuation. ──
	subject := subjectKey(rel)
	var joined *chain
	if c := r.chains[hit.Author.ID]; !detected && c != nil && c.subject == subject &&
		hit.CreatedAt.Sub(c.lastAt) <= bot.ChainWindow && c.attached < bot.ChainMaxFollowUps {
		joined = c
		c.attached++
		log.Printf("   ↪  re-ping %s after the last message of %s — joins that set",
			hit.CreatedAt.Sub(c.lastAt).Round(time.Second), c.rootLink)
		c.lastAt = hit.CreatedAt
	}

	// ── Who is credited ──
	uploader := meta.Uploader
	if uploader == "" {
		uploader = bot.UploaderFromMessage(hit)
	}
	upID, skipReason, err := r.dir.lookupOrCreateUploader(r.pb, uploader, r.commit)
	if err != nil {
		st.failed++
		log.Printf("   ❌ uploader %q: %v", uploader, err)
		return
	}
	if skipReason != "" {
		skip(fmt.Sprintf("uploader %s %s", uploader, skipReason))
		return
	}
	var tagIDs []string
	for _, t := range bot.SplitTrim(meta.Tags) {
		id, err := r.dir.lookupOrCreateTag(r.pb, t, r.commit)
		if err != nil {
			st.failed++
			log.Printf("   ❌ tag %q: %v", t, err)
			return
		}
		tagIDs = append(tagIDs, id)
	}

	// ── Items, across the hit and its replies, minus what is already archived ──
	//
	// Idempotency lives here. Every record the backfill (or the live bot, or
	// /reupload) creates carries its message's jump link in `discord`, so the
	// archive itself is the ledger: a rerun reads the records for each message
	// and plans only the items that have none. An item is matched to a record by
	// its imgur mirror when it has one and by filename otherwise — the two
	// things a record keeps that trace back to its source — with a count-based
	// shortcut for the common case of a message that landed whole. A record with
	// no `original` is a shell an earlier run died behind (created, never
	// finished); it is removed and its item redone rather than left as an item
	// that never finishes.
	//
	// The one case this can get wrong: a partly failed message whose finished
	// item was stored under a filename the host renamed on download (pixeldrain
	// does this). The filename match then misses, and the retry duplicates that
	// one item. Rare enough to accept; two runs at once is the thing to avoid.
	// ── Bare follow-ups: the author's next unpinged messages with media, within
	// the window, up to the chain's remaining budget (bot.chainContinuation). ──
	extra := append([]discord.Message{}, replies...)
	var followups []discord.Message
	if r.followups {
		budget := bot.ChainMaxFollowUps
		if joined != nil {
			budget -= joined.attached
		}
		inSet := map[snowflake.ID]bool{hit.ID: true}
		for _, m := range replies {
			inSet[m.ID] = true
		}
		fus, err := r.dc.followUps(ctx, hit, inSet, budget)
		if err != nil {
			st.failed++
			log.Printf("   ❌ follow-up scan: %v", err)
			return
		}
		followups = fus
		extra = append(extra, fus...)
		for _, m := range fus {
			log.Printf("   ＋ follow-up %s (%s later)", jumpLink(guildID, m), m.CreatedAt.Sub(hit.CreatedAt).Round(time.Second))
		}
	}

	for _, m := range extra {
		r.consumed[m.ID] = true
	}

	var items []plannedItem
	joinSetID := ""
	archivedItems := 0
	for _, m := range append([]discord.Message{hit}, extra...) {
		mlink := jumpLink(guildID, m)
		existing, err := r.pb.listAll("contents", "discord = "+pbQuote(mlink), "id,set,mirror,filename,original")
		if err != nil {
			st.failed++
			log.Printf("   ❌ dedupe lookup: %v", err)
			return
		}
		var finished []pbRecord
		for _, rec := range existing {
			if joinSetID == "" {
				joinSetID = rec.str("set")
			}
			if rec.str("original") == "" {
				log.Printf("   ♻️  %s: record %s was never finished by an earlier run — redoing it", mlink, rec.str("id"))
				if r.commit {
					if err := r.pb.delete("contents", rec.str("id")); err != nil {
						st.failed++
						log.Printf("   ❌ could not remove unfinished record %s: %v", rec.str("id"), err)
						return
					}
				}
				continue
			}
			finished = append(finished, rec)
		}
		found := bot.CollectMedia(m)
		if len(finished) >= len(found) {
			archivedItems += len(found)
			continue
		}
		for _, it := range found {
			if matchesExisting(it, finished) {
				archivedItems++
				continue
			}
			items = append(items, plannedItem{msg: m, item: it})
		}
	}
	st.alreadyArchived += archivedItems
	if joinSetID == "" && joined != nil {
		joinSetID = joined.setID
	}

	// Whatever happens below, this ping is now the author's open chain (or
	// extends it): its follow-ups count against the budget, the clock runs from
	// the last message that joined, and a later re-ping joins whatever set these
	// items are in — including one an earlier run already made.
	lastAt := hit.CreatedAt
	for _, m := range followups {
		if m.CreatedAt.After(lastAt) {
			lastAt = m.CreatedAt
		}
	}
	c := joined
	if c == nil {
		c = &chain{subject: subject, rootLink: link}
		r.chains[hit.Author.ID] = c
	}
	c.attached += len(followups)
	c.lastAt = lastAt
	if c.setID == "" {
		c.setID = joinSetID
	}

	if len(items) == 0 {
		if archivedItems > 0 {
			skip(fmt.Sprintf("already archived (%d item%s)", archivedItems, plural(archivedItems, "", "s")))
		} else {
			skip("no ingestible media")
		}
		return
	}
	if archivedItems > 0 {
		log.Printf("   ℹ️  %d item%s already archived; planning the rest", archivedItems, plural(archivedItems, "", "s"))
	}
	plan := setPlan{
		hit: hit, meta: meta, rel: rel,
		idolName: idolNames, grpName: groupNames,
		uploader: uploader, upID: upID, tagIDs: tagIDs, items: items,
	}

	log.Printf("   %s by %s · %s · %s → %s", hit.CreatedAt.UTC().Format("2006-01-02"),
		uploader, link, meta.Title, describe(plan))
	if len(rel.missing) > 0 {
		log.Printf("   ⚠️  unresolved names skipped: %s", strings.Join(rel.missing, ", "))
	}

	if !r.commit {
		r.dryRun(plan, st)
		return
	}
	if setID := r.commitSet(ctx, plan, joinSetID, st); setID != "" {
		c.setID = setID
	}
}

// detectFromText is bot.textDetection: the idol and group a message names in
// its own words, plus the pinged role names so a group-only ping still
// contributes its group. Every hit is logged with the string that matched,
// because this is the one rule that attributes something nobody stated.
func (r *runner) detectFromText(hit discord.Message, roleNames []string) (bot.Metadata, bool) {
	content := hit.Content
	if len(roleNames) > 0 {
		content += "\n" + strings.Join(roleNames, "\n")
	}
	idols, groups, matched, ok := r.dir.detector().Detect(content)
	if !ok {
		return bot.Metadata{}, false
	}
	meta := bot.Metadata{Idol: strings.Join(idols, ", "), Group: strings.Join(groups, ", ")}
	// `key: value` lines still win — someone who wrote them meant them.
	if err := bot.ExtractMetadata(hit.Content, &meta); err != nil {
		return bot.Metadata{}, false
	}
	log.Printf("   🔎 named in the text, not by a role: %s [%s] (matched: %s)",
		strings.Join(idols, ", "), strings.Join(groups, ", "), strings.Join(matched, ", "))
	return meta, true
}

// subjectKey is sameSubject's comparison for resolved ids: order-independent,
// so the same roles pinged in a different order are the same drop.
func subjectKey(rel resolved) string {
	idols := append([]string{}, rel.idolIDs...)
	groups := append([]string{}, rel.groupIDs...)
	sort.Strings(idols)
	sort.Strings(groups)
	return strings.Join(idols, ",") + "|" + strings.Join(groups, ",")
}

// matchesExisting reports whether one of the finished records for a message is
// this item: same imgur mirror, or (for anything without one) same filename.
func matchesExisting(it bot.MediaItem, finished []pbRecord) bool {
	for _, rec := range finished {
		if it.Mirror != "" {
			if rec.str("mirror") == it.Mirror {
				return true
			}
			continue
		}
		if it.Filename != "" && rec.str("filename") == it.Filename {
			return true
		}
	}
	return false
}

func describe(p setPlan) string {
	var kinds []string
	att, links := 0, 0
	for _, it := range p.items {
		if strings.Contains(it.item.URL, "cdn.discordapp.com") || strings.Contains(it.item.URL, "media.discordapp.net") {
			att++
		} else {
			links++
		}
	}
	if att > 0 {
		kinds = append(kinds, fmt.Sprintf("%d attachment%s", att, plural(att, "", "s")))
	}
	if links > 0 {
		kinds = append(kinds, fmt.Sprintf("%d link%s", links, plural(links, "", "s")))
	}
	extra := 0
	for _, it := range p.items {
		if it.msg.ID != p.hit.ID {
			extra++
		}
	}
	s := strings.Join(kinds, ", ")
	if extra > 0 {
		s += fmt.Sprintf(" (%d from replies/follow-ups)", extra)
	}
	return s
}

// dryRun reports what commit would do, optionally probing each link.
func (r *runner) dryRun(p setPlan, st *stats) {
	st.sets++
	for _, it := range p.items {
		line := "      · " + it.item.URL
		if r.probe {
			if err := probe(it.item.URL); errors.Is(err, errUnretrievable) {
				line += "  ✗ gone (" + strings.TrimPrefix(err.Error(), errUnretrievable.Error()+": ") + ")"
				st.dead++
			} else if err != nil {
				line += "  ? " + err.Error()
			} else {
				line += "  ✓"
				st.created++
			}
		} else {
			st.created++
		}
		log.Print(line)
	}
}

// commitSet writes one set: the set record, then every item in parallel.
// Returns the set the items went into, "" if none survived.
func (r *runner) commitSet(ctx context.Context, p setPlan, joinSetID string, st *stats) string {
	created := p.hit.CreatedAt.UTC()
	setID := joinSetID
	ownSet := false

	// A set for everything but stickers, as runIngestion does; the site's main
	// listing reads sets, so a setless item would be invisible there.
	if setID == "" && p.meta.Filetype != "sticker" {
		rec, err := r.pb.create("contents_sets", map[string]any{
			"title":    created.Format("060102") + " " + p.meta.Title,
			"idol":     p.rel.idolIDs,
			"group":    p.rel.groupIDs,
			"uploader": []string{p.upID},
			"date":     created.Format(pbTime),
		}, created, r.origin)
		if err != nil {
			st.failed += len(p.items)
			log.Printf("   ❌ set: %v", err)
			return ""
		}
		setID = rec.str("id")
		ownSet = true
		r.verifyProvenance(setID, created)
		log.Printf("   📁 set %s", setID)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ok, bad int
		dead    int
	)
	for _, it := range p.items {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		r.sem <- struct{}{}
		go func(it plannedItem) {
			defer wg.Done()
			defer func() { <-r.sem }()
			err := r.commitItem(p, it, setID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, errUnretrievable):
				dead++
				log.Printf("   ✗ gone: %s (%v)", it.item.URL, err)
			default:
				bad++
				log.Printf("   ❌ %s: %v", it.item.URL, err)
			}
		}(it)
	}
	wg.Wait()

	st.created += ok
	st.failed += bad
	st.dead += dead
	if ok > 0 {
		st.sets++
		log.Printf("   ✅ %d/%d item%s archived", ok, len(p.items), plural(len(p.items), "", "s"))
		return setID
	}
	if ownSet {
		// Nothing landed: don't leave an empty set on the front page.
		if err := r.pb.delete("contents_sets", setID); err != nil {
			log.Printf("   ⚠️  could not remove empty set %s: %v", setID, err)
		}
		setID = ""
	}
	if bad == 0 {
		st.skipped++
	}
	return setID
}

// commitItem is one content record end to end: download, classify, create the
// shell, encode, upload, point the record at the objects. Anything after the
// shell exists is rolled back on failure so a half-done item never shows on the
// site as one that "never finishes".
func (r *runner) commitItem(p setPlan, it plannedItem, setID string) error {
	// The message's time is both the content's `date` and its `created` — the
	// latter through the provenance hook, since the API otherwise stamps "now".
	created := it.msg.CreatedAt.UTC()
	mlink := jumpLink(guildID, it.msg)

	f, err := download(it.item.URL)
	if err != nil {
		return err
	}

	// Filetype precedence as in bot.createContentRecord, then the byte-level
	// still/animation correction the server's create hook applies.
	filetype := p.meta.Filetype
	if filetype == "" {
		filetype = it.item.Filetype
	}
	if filetype == "" {
		filetype = filetypeByContentType(f.contentType)
	}
	if filetype == "" {
		return fmt.Errorf("cannot determine filetype (content-type %q)", f.contentType)
	}
	filename := it.item.Filename
	if f.filename != "" {
		filename = f.filename
	}
	if path.Ext(filename) == "" {
		if ext := extFromContentType(f.contentType); ext != "" {
			filename += ext
		}
	}
	ext := strings.ToLower(path.Ext(filename))
	if filetype == "image" || filetype == "gif" {
		if detected, err := hooks.ClassifyBytes(f.data, ext); err == nil && detected != filetype {
			log.Printf("   ℹ️  %s hinted %s, bytes say %s", filename, filetype, detected)
			filetype = detected
		}
	}

	// Keep the source. The server pipeline deletes its input after encoding,
	// which is why every quality change there is forward-only; this one isn't.
	if r.workdir != "" {
		dir := filepath.Join(r.workdir, it.msg.ID.String())
		if err := os.MkdirAll(dir, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(dir, filename), f.data, 0o644)
		}
	}

	mirror := p.meta.Mirror
	if mirror == "" {
		mirror = it.item.Mirror
	}

	// ── Shell record: metadata only, no file, so the server's encode hook
	// stays out of it. ──
	shell := map[string]any{
		"title":          p.meta.Title,
		"idol":           p.rel.idolIDs,
		"group":          p.rel.groupIDs,
		"uploader":       p.upID,
		"tag":            p.tagIDs,
		"filetype":       filetype,
		"preview_format": r.previewFmt,
		"date":           created.Format(pbTime),
		"source":         p.meta.Source,
		"discord":        mlink,
		"mirror":         mirror,
		"filename":       filename,
	}
	if setID != "" {
		shell["set"] = setID
	}
	rec, err := r.pb.create("contents", shell, created, r.origin)
	if err != nil {
		return fmt.Errorf("create record: %w", err)
	}
	id := rec.str("id")

	rollback := func(keys []string, why error) error {
		for _, k := range keys {
			if derr := r.fs.Delete(k); derr != nil {
				log.Printf("   ⚠️  rollback: could not delete %s: %v", k, derr)
			}
		}
		if derr := r.pb.delete("contents", id); derr != nil {
			log.Printf("   ⚠️  rollback: could not delete record %s: %v", id, derr)
		}
		return why
	}

	// ── Encode, here ──
	tags := hooks.RenditionTags(id, p.meta.Title, filename, []string{p.uploader}, p.idolName, p.grpName, created, created)
	started := time.Now()
	rend, err := hooks.EncodeRenditions(f.data, ext, filetype, r.previewFmt, tags)
	if err != nil {
		return rollback(nil, fmt.Errorf("encode: %w", err))
	}

	// ── Upload under the keys the server would have chosen ──
	keyBase := hooks.ContentKeyBase(filetype, slugs(p.grpName), slugs(p.idolName), created.Format("060102"), id)
	var uploaded []string
	put := func(rd *hooks.Rendition) (string, error) {
		if rd == nil {
			return "", nil
		}
		key := keyBase + rd.Suffix
		if err := r.fs.Upload(rd.Bytes, key); err != nil {
			return "", fmt.Errorf("upload %s: %w", key, err)
		}
		uploaded = append(uploaded, key)
		return r.publicURL + "/" + key, nil
	}
	originalURL, err := put(&rend.Original)
	if err != nil {
		return rollback(uploaded, err)
	}
	fields := map[string]any{
		"original": originalURL,
		"preview":  originalURL,
		"width":    rend.Width,
		"height":   rend.Height,
	}
	// The rest are best-effort, as on the server: a record with a working
	// original and no fallback is degraded, not lost.
	for name, rd := range map[string]*hooks.Rendition{"preview": rend.Preview, "static": rend.Static, "sd": rend.SD} {
		u, err := put(rd)
		if err != nil {
			log.Printf("   ⚠️  %s: %v", name, err)
			continue
		}
		if u != "" {
			fields[name] = u
		}
	}
	if _, err := r.pb.update("contents", id, fields); err != nil {
		return rollback(uploaded, fmt.Errorf("write urls: %w", err))
	}

	log.Printf("   ✓ %s → %s (%s, %s)", filename, originalURL, filetype, time.Since(started).Round(time.Second))
	return nil
}

// verifyProvenance reads the first set back and checks that the server kept
// `created` and `origin` — i.e. that hooks/provenance.go is deployed. Once per
// run: if the first record is wrong, every record would be wrong the same way,
// so the run stops here, with the test set removed.
func (r *runner) verifyProvenance(setID string, want time.Time) {
	r.verified.Do(func() {
		rec, err := r.pb.get("contents_sets", setID, "id,created,origin")
		if err != nil {
			log.Fatalf("❌ could not read back set %s: %v", setID, err)
		}
		got, perr := time.Parse(pbTime, rec.str("created"))
		if perr != nil || got.Sub(want).Abs() > 2*time.Second || rec.str("origin") != r.origin {
			if derr := r.pb.delete("contents_sets", setID); derr != nil {
				log.Printf("⚠️  could not remove the test set %s: %v", setID, derr)
			}
			log.Fatalf("❌ the server did not keep provenance (sent created %s / origin %s, got %q / %q) — "+
				"is hooks/provenance.go deployed, with %q in the origin select? Aborting before anything else is written.",
				want.Format(pbTime), r.origin, rec.str("created"), rec.str("origin"), r.origin)
		}
		log.Printf("   ✔ server honours created and origin (%s, %s)", got.Format("2006-01-02 15:04"), rec.str("origin"))
	})
}

func slugs(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, hooks.Slugify(n))
	}
	return out
}

// The two Content-Type helpers below mirror bot/links.go (unexported there;
// small enough that a wrapper would be longer than the copy).

func filetypeByContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch {
	case ct == "image/gif":
		return "gif"
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "video/"):
		return "gif"
	}
	return ""
}

func extFromContentType(ct string) string {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/avif":
		return ".avif"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	}
	return ""
}

// openR2 builds the S3 client from the environment. Credentials live in
// PocketBase's settings on the server; the script needs its own copy.
func openR2() (*filesystem.System, error) {
	get := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	endpoint, bucket, key, secret := get("R2_ENDPOINT"), get("R2_BUCKET"), get("R2_ACCESS_KEY"), get("R2_SECRET_KEY")
	if endpoint == "" || bucket == "" || key == "" || secret == "" {
		return nil, fmt.Errorf("R2_ENDPOINT, R2_BUCKET, R2_ACCESS_KEY and R2_SECRET_KEY are required for -commit")
	}
	region := get("R2_REGION")
	if region == "" {
		region = "auto"
	}
	return filesystem.NewS3(bucket, region, endpoint, key, secret, false)
}
