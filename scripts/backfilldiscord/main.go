// Command backfilldiscord archives the Discord posts that pinged one idol role
// before the bot existed, encoding them on this machine and handing the server
// finished renditions.
//
// It is a standalone process: Discord over REST with the bot token (no gateway,
// so the live bot is undisturbed), PocketBase over HTTP as a superuser, R2 over
// S3 with its own credentials. The server never sees a file — records are
// created with the rendition URLs already filled in, which is the one shape its
// encode hook ignores (hooks/r2.go, OnRecordAfterCreateSuccess bails on an
// empty `file`). Nothing on the server has to change or restart.
//
// Per role it does what the live bot's role-ping trigger would have done to each
// message, using the same parsers (bot.CollectMedia, bot.RolesToMetadata,
// bot.ExtractMetadata) and the same encoders (hooks.EncodeRenditions), so the
// archive can't tell a backfilled item from a live one — except for two things
// the API refuses to let a caller set:
//
//   - `created` and the origin need hooks/provenance.go on the server, which
//     lets a superuser state both. The first set written is read back
//     to check the hook is deployed; without it the run stops there.
//   - the encode-time Discord notices ("AVIF ready", upload announcement) never
//     fire, because the server never encodes. Intended.
//
// Dry run by default. -commit writes. Safe to rerun: the archive is its own
// ledger — records carry their message's jump link in `contents.discord`, and a
// rerun plans only the items of each message that have no finished record,
// removing any shell an earlier run died behind. See processHit.
//
//	go run ./scripts/backfilldiscord -role 1234567890 -url https://api.example.com
//	go run ./scripts/backfilldiscord -role 1234567890 -url https://api.example.com -probe
//	go run ./scripts/backfilldiscord -role 1234567890 -url https://api.example.com -commit -limit 5
//
// Records are stamped origin "script" — `discord` with a handle on it, so the
// backfill can be reviewed as a whole and, once trusted, relabelled:
//
//	go run ./scripts/backfilldiscord -url https://api.example.com -relabel script:discord -commit
//
// Environment (a .env in the working directory is loaded):
//
//	DISCORD_TOKEN                  bot token (REST only)
//	IMGUR_CLIENT_ID                imgur API client id (default: imgur's own web client id)
//	PB_ADMIN_EMAIL / PB_ADMIN_PASSWORD   superuser
//	R2_ENDPOINT, R2_BUCKET, R2_ACCESS_KEY, R2_SECRET_KEY, R2_REGION (default auto)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"goyangi-v1-be/bot"
	"goyangi-v1-be/hooks"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/joho/godotenv"
	"github.com/pocketbase/pocketbase/tools/filesystem"
)

// Where the archive comes from. Fixed rather than configured: the backfill is a
// one-off against one known server and one known channel, and the live bot's
// own defaults (DISCORD_GUILD_ID, DISCORD_ALLOWED_CHANNEL_IDS) describe where it
// announces and watches today, which is not the same place.
const guildID snowflake.ID = 124767749099618304

var channelIDs = []snowflake.ID{124767749099618304}

// publicURL is the CDN the production bucket is served from. Fixed for the
// same reason as the bucket's guild and channel — and NOT read from the
// environment: a development .env carries the dev CDN's URL, and a record
// written with that host would point at objects that aren't there.
const publicURL = "https://cdn.goyangi.pics"

// botLaunch is the day the live bot started ingesting. Nothing on or after it is
// the backfill's to touch.
var botLaunch = time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)

func main() {
	log.SetFlags(log.Ltime)
	if err := godotenv.Load(); err == nil {
		log.Printf("ℹ️  loaded .env")
	}

	var (
		roleRaw     = flag.String("role", "", "role id whose pings to backfill (required)")
		baseURL     = flag.String("url", "http://127.0.0.1:8090", "PocketBase base URL")
		email       = flag.String("email", os.Getenv("PB_ADMIN_EMAIL"), "superuser email (prefer PB_ADMIN_EMAIL)")
		password    = flag.String("password", os.Getenv("PB_ADMIN_PASSWORD"), "superuser password (prefer PB_ADMIN_PASSWORD)")
		commit      = flag.Bool("commit", false, "write records and upload renditions (default: report only)")
		probe       = flag.Bool("probe", false, "dry run: also check whether each link still downloads")
		limit       = flag.Int("limit", 0, "stop after this many sets (0 = all)")
		before      = flag.String("before", "", "only messages before this day (YYMMDD, exclusive)")
		after       = flag.String("after", "", "only messages on or after this day (YYMMDD)")
		concurrency = flag.Int("concurrency", 2, "items encoded in parallel")
		previewFmt  = flag.String("preview", "webp", "animated preview format for gifs: webp (what the live bot uses) or avif")
		workdir     = flag.String("workdir", "backfill-work", "where downloaded sources are kept (one folder per message)")
		replies     = flag.Bool("replies", true, "also collect same-author replies to each pinged message into its set")
		followups   = flag.Bool("followups", true, "also collect the author's bare follow-up messages (the live bot's chain rule) into the set")
		delay       = flag.Duration("delay", 0, "pause between sets")
		ffmpegDir   = flag.String("ffmpeg", "", "directory holding the ffmpeg/ffprobe to use (default: PATH, then Homebrew's keg-only ffmpeg-full)")
		origin      = flag.String("origin", hooks.OriginScript, "origin to stamp on the records: script (reversible marker) or discord")
		relabel     = flag.String("relabel", "", "instead of backfilling, relabel every record's origin FROM:TO (e.g. script:discord) and exit")
		scan        = flag.Bool("scan", false, "gap mode: instead of one role's pings, read the channel's history in the window and archive every role-pinged message (replaces -role; needs both -after and -before, each YYMMDD or an RFC3339 timestamp, and may reach past the bot's launch)")
	)
	flag.Parse()

	if *relabel != "" {
		runRelabel(*baseURL, *email, *password, *relabel, *commit)
		return
	}
	if *scan {
		if *roleRaw != "" {
			log.Fatal("-scan reads every role ping in the window; drop -role")
		}
		if *after == "" || *before == "" {
			log.Fatal("-scan needs both -after and -before: it is allowed past the bot's launch, so the window has to be stated")
		}
	} else if *roleRaw == "" {
		log.Fatal("-role is required (or -scan for a gap fill)")
	}
	if *origin != hooks.OriginScript && *origin != hooks.OriginDiscord {
		log.Fatal("-origin must be script or discord")
	}
	var roleID snowflake.ID
	if !*scan {
		var err error
		roleID, err = snowflake.Parse(*roleRaw)
		if err != nil {
			log.Fatalf("-role: %v", err)
		}
	}
	if *previewFmt != "webp" && *previewFmt != "avif" {
		log.Fatal("-preview must be webp or avif")
	}
	if *concurrency < 1 {
		log.Fatal("-concurrency must be at least 1")
	}
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN is required")
	}
	if *email == "" || *password == "" {
		log.Fatal("superuser credentials required: PB_ADMIN_EMAIL and PB_ADMIN_PASSWORD, or -email/-password")
	}

	// The live bot has ingested everything since it launched, so a per-role
	// search never reaches past that day: -before can narrow the window, not
	// widen it. -scan is the exception — it exists to fill a stretch the live bot
	// was down for, so it takes the window it is given (both ends required above)
	// and leans on the per-message dedupe for any overlap with what the bot did
	// ingest.
	var minID snowflake.ID
	maxID := snowflake.New(botLaunch)
	ceiling := botLaunch
	if *after != "" {
		t, err := parseWindowTime(*after)
		if err != nil {
			log.Fatalf("-after: %v", err)
		}
		minID = snowflake.New(t)
	}
	if *before != "" {
		t, err := parseWindowTime(*before)
		if err != nil {
			log.Fatalf("-before: %v", err)
		}
		if t.After(botLaunch) && !*scan {
			log.Fatalf("-before %s is after the bot's launch (%s); everything from launch on is already the live bot's",
				*before, botLaunch.Format("060102"))
		}
		maxID = snowflake.New(t)
		if *scan {
			ceiling = t
		}
	}
	if *scan && minID.Time().After(maxID.Time()) {
		log.Fatal("-after is later than -before")
	}
	if *scan {
		log.Printf("📅 window: %s → %s (exclusive)", minID.Time().UTC().Format(time.RFC3339), maxID.Time().UTC().Format(time.RFC3339))
	} else {
		log.Printf("📅 window: %s → %s (exclusive)", orAny(*after), maxIDDay(maxID))
	}

	checkTools(*previewFmt, *ffmpegDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── PocketBase ──
	pb := newPBClient(*baseURL)
	if err := pb.authenticate(*email, *password); err != nil {
		log.Fatalf("PocketBase auth failed: %v", err)
	}
	log.Printf("🔑 PocketBase: %s", pb.baseURL)
	dir, err := loadDirectory(pb)
	if err != nil {
		log.Fatalf("directory: %v", err)
	}
	log.Printf("📖 directory: %d groups, %d idols, %d uploaders, %d tags",
		len(dir.groups), len(dir.idols), len(dir.uploaders), len(dir.tags))

	// ── Discord ──
	dc, err := newDiscord(token)
	if err != nil {
		log.Fatalf("Discord: %v", err)
	}
	dc.ceiling = ceiling
	var roleName string
	if *scan {
		log.Printf("🎯 every role ping in channel %s", channelIDs[0])
	} else {
		var ok bool
		roleName, ok = dc.roles[roleID]
		if !ok {
			log.Fatalf("role %s is not in guild %s", roleID, guildID)
		}
		log.Printf("🎯 role %q in channel %s", roleName, channelIDs[0])
	}

	// ── R2 (commit only) ──
	var fs *filesystem.System
	if *commit {
		fs, err = openR2()
		if err != nil {
			log.Fatalf("R2: %v", err)
		}
		defer fs.Close()
		log.Printf("☁️  R2: %s", publicURL)
	} else {
		log.Printf("🔍 DRY RUN — nothing will be written. Pass -commit to apply.")
	}

	r := &runner{
		pb: pb, dc: dc, dir: dir, fs: fs,
		publicURL:  publicURL,
		commit:     *commit,
		probe:      *probe,
		previewFmt: *previewFmt,
		workdir:    *workdir,
		followups:  *followups,
		origin:     *origin,
		sem:        make(chan struct{}, *concurrency),
		chains:     map[snowflake.ID]*chain{},
		consumed:   map[snowflake.ID]bool{},
		replies:    *replies,
	}

	// ── Search ──
	log.Printf("🔎 searching…")
	var hits []discord.Message
	loose := map[snowflake.ID]bool{} // scan only: media with no ping, left to text detection
	if *scan {
		all, err := dc.scanChannel(ctx, channelIDs, minID, maxID)
		if err != nil {
			log.Fatalf("search: scan: %v", err)
		}
		for _, m := range all {
			switch {
			case m.Author.System || m.Author.ID == dc.appID:
			case len(m.MentionRoles) > 0 || bot.BotMentionedIn(m.Content, dc.appID.String()):
				hits = append(hits, m)
			case len(bot.CollectMedia(m)) > 0:
				hits = append(hits, m)
				loose[m.ID] = true
			}
		}
		log.Printf("📬 %d message(s) ping a role or the bot, and %d more carry media with no ping (text detection decides those)",
			len(hits)-len(loose), len(loose))
		log.Printf("🔤 text detection: %d name%s suppressed by GOYANGI_DETECT_STOPWORDS",
			bot.DetectStopwordCount(), plural(bot.DetectStopwordCount(), "", "s"))
	} else {
		hits, err = dc.searchAll(ctx, searchQuery{
			mentionsRoleIDs: []snowflake.ID{roleID},
			channelIDs:      channelIDs,
			minID:           minID,
			maxID:           maxID,
		})
		if err != nil {
			log.Fatalf("search: %v", err)
		}
		log.Printf("📬 %d message(s) ping %q", len(hits), roleName)
	}

	var replyMap map[snowflake.ID][]discord.Message
	if *replies && len(hits) > 0 {
		pinged := hits
		if *scan {
			pinged = nil
			for _, h := range hits {
				if !loose[h.ID] {
					pinged = append(pinged, h)
				}
			}
		}
		replyMap, err = dc.repliesTo(ctx, pinged)
		if err != nil {
			log.Fatalf("replies: %v", err)
		}
		n := 0
		for _, rs := range replyMap {
			n += len(rs)
		}
		log.Printf("↩️  %d same-author repl%s found", n, plural(n, "y", "ies"))
	}

	// ── Process ──
	var st stats
	for i, hit := range hits {
		if ctx.Err() != nil {
			log.Printf("⏹  interrupted")
			break
		}
		if *limit > 0 && st.sets >= *limit {
			log.Printf("⏹  -limit %d reached", *limit)
			break
		}
		if loose[hit.ID] && r.consumed[hit.ID] {
			continue // already part of an earlier set as a reply or follow-up
		}
		log.Printf("── [%d/%d] %s", i+1, len(hits), jumpLink(guildID, hit))
		r.processHit(ctx, hit, replyMap[hit.ID], &st)
		if *delay > 0 && *commit {
			time.Sleep(*delay)
		}
	}

	log.Printf("── done: %d set%s, %d item%s created, %d item%s failed, %d message%s skipped (%d item%s already archived, %d unretrievable link%s)",
		st.sets, plural(st.sets, "", "s"),
		st.created, plural(st.created, "", "s"),
		st.failed, plural(st.failed, "", "s"),
		st.skipped, plural(st.skipped, "", "s"),
		st.alreadyArchived, plural(st.alreadyArchived, "", "s"), st.dead, plural(st.dead, "", "s"))
	if st.failed > 0 {
		os.Exit(1)
	}
}

type stats struct {
	sets, created, failed, skipped, alreadyArchived, dead int
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func jumpLink(guildID snowflake.ID, m discord.Message) string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, m.ChannelID, m.ID)
}

func orAny(s string) string {
	if s == "" {
		return "beginning"
	}
	return s
}

func maxIDDay(id snowflake.ID) string {
	return id.Time().UTC().Format("060102")
}

// kegOnlyFFmpeg is where Homebrew keeps ffmpeg-full, the build that has
// libwebp. Homebrew's plain ffmpeg bottle doesn't, and this machine has both.
const kegOnlyFFmpeg = "/opt/homebrew/opt/ffmpeg-full/bin"

// checkTools refuses to start without the encoders the pipeline shells out to.
// A missing encoder would otherwise surface as a per-item failure after the
// download, once per item.
//
// The encoders are found through PATH (hooks/exec.go), so choosing a build means
// putting its directory first on PATH for this process. An explicit -ffmpeg
// wins; otherwise the PATH build is tried, and if it lacks an encoder the
// keg-only ffmpeg-full is tried in its place.
func checkTools(previewFmt, ffmpegDir string) {
	need := []string{"libsvtav1", "libx264"}
	if previewFmt == "webp" {
		need = append(need, "libwebp")
	}
	candidates := []string{ffmpegDir}
	if ffmpegDir == "" {
		candidates = []string{"", kegOnlyFFmpeg}
	}
	var lastErr error
	for _, dir := range candidates {
		if dir != "" {
			if _, err := os.Stat(dir); err != nil {
				lastErr = fmt.Errorf("%s: %v", dir, err)
				continue
			}
			os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
		if lastErr = encodersPresent(need); lastErr == nil {
			bin, _ := exec.LookPath("ffmpeg")
			log.Printf("🎬 %s", bin)
			break
		}
	}
	if lastErr != nil {
		log.Fatalf("no usable ffmpeg: %v (libwebp is needed for -preview webp; pass -preview avif or -ffmpeg <dir>)", lastErr)
	}
	// hooks/h264.go picks the SD encoder (VAAPI, NVENC or libx264) on the
	// first encode and logs which one it chose.
}

// encodersPresent reports whether the ffmpeg on PATH has every named encoder.
func encodersPresent(need []string) error {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found in PATH", bin)
		}
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		return fmt.Errorf("ffmpeg -encoders: %v", err)
	}
	for _, enc := range need {
		if !strings.Contains(string(out), " "+enc+" ") {
			return fmt.Errorf("ffmpeg lacks the %s encoder", enc)
		}
	}
	return nil
}

// ─── Discord ─────────────────────────────────────────────────────────────────

// discordREST is the REST-only client plus the guild facts every message needs.
type discordREST struct {
	client rest.Client
	rest   rest.Rest
	appID  snowflake.ID
	roles  map[snowflake.ID]string

	// ceiling is where follow-up scans stop: messages at or after it belong to
	// someone else (the live bot, or whatever lies past a -scan window).
	ceiling time.Time
}

func newDiscord(token string) (*discordREST, error) {
	client := rest.NewClient(token)
	r := rest.New(client)
	app, err := r.GetCurrentApplication()
	if err != nil {
		return nil, fmt.Errorf("who am I: %w", err)
	}
	roles, err := r.GetRoles(guildID)
	if err != nil {
		return nil, fmt.Errorf("roles: %w", err)
	}
	d := &discordREST{client: client, rest: r, appID: app.ID, roles: map[snowflake.ID]string{}, ceiling: botLaunch}
	for _, role := range roles {
		d.roles[role.ID] = role.Name
	}
	return d, nil
}

// searchQuery is the slice of the search endpoint's filters the backfill uses.
//
// Encoded here rather than through disgo's GuildMessagesSearch: that joins list
// parameters with commas ("channel_id=a,b"), and Discord rejects the result as
// "not snowflake". The endpoint wants the key repeated
// ("channel_id=a&channel_id=b"), which url.Values.Add produces.
type searchQuery struct {
	mentionsRoleIDs     []snowflake.ID
	channelIDs          []snowflake.ID
	repliedToMessageIDs []snowflake.ID
	minID, maxID        snowflake.ID
	offset              int
}

const searchPageSize = 25

func (q searchQuery) values() url.Values {
	v := url.Values{}
	v.Set("limit", strconv.Itoa(searchPageSize))
	v.Set("sort_by", "timestamp")
	v.Set("sort_order", "asc")
	if q.offset > 0 {
		v.Set("offset", strconv.Itoa(q.offset))
	}
	if q.minID != 0 {
		v.Set("min_id", q.minID.String())
	}
	if q.maxID != 0 {
		v.Set("max_id", q.maxID.String())
	}
	for _, id := range q.channelIDs {
		v.Add("channel_id", id.String())
	}
	for _, id := range q.mentionsRoleIDs {
		v.Add("mentions_role_id", id.String())
	}
	for _, id := range q.repliedToMessageIDs {
		v.Add("replied_to_message_id", id.String())
	}
	return v
}

// search is one page. A guild that isn't indexed yet answers 202 with a
// retry-after, which is waited out here the way disgo's own wrapper does.
func (d *discordREST) search(ctx context.Context, q searchQuery) (*discord.GuildMessagesSearchResult, error) {
	ep := rest.SearchGuildMessages.Compile(nil, guildID)
	ep.URL += "?" + q.values().Encode()
	for {
		var res discord.GuildMessagesSearchResult
		err := withRetry(ctx, func() error {
			res = discord.GuildMessagesSearchResult{}
			return d.client.Do(ep, nil, &res, rest.WithCtx(ctx))
		})
		if err != nil {
			return nil, err
		}
		if res.Code != int(rest.JSONErrorCodeIndexNotYetAvailable) {
			return &res, nil
		}
		wait := time.Duration(res.RetryAfter) * time.Second
		if wait <= 0 {
			wait = time.Second
		}
		log.Printf("⏳ Discord is still indexing the server (%d documents so far) — retrying in %s", res.DocumentsIndexed, wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// withRetry runs a Discord REST call, retrying a handful of times on a 5xx —
// the search and history endpoints answer with the occasional 503/504 under
// load, and a single one used to fail the whole message. Anything else (4xx,
// context cancelled) returns at once.
func withRetry(ctx context.Context, call func() error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if err = call(); err == nil {
			return nil
		}
		var restErr *rest.Error
		if !errors.As(err, &restErr) || restErr.Response == nil || restErr.Response.StatusCode < 500 {
			return err
		}
		wait := time.Duration(2<<attempt) * time.Second
		log.Printf("   ⏳ Discord %d — retrying in %s", restErr.Response.StatusCode, wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return err
}

// searchAll walks every page of a search, oldest first.
//
// Discord pages by offset up to 9975; past that the window is moved forward by
// setting min_id to the last message seen, which is why results are sorted by
// timestamp ascending rather than relevance. Results are de-duplicated by id
// because the two mechanisms can overlap by a message at the seam.
func (d *discordREST) searchAll(ctx context.Context, q searchQuery) ([]discord.Message, error) {
	const maxOffset = 9975
	var out []discord.Message
	seen := map[snowflake.ID]bool{}
	for {
		res, err := d.search(ctx, q)
		if err != nil {
			return nil, err
		}
		if len(res.Messages) == 0 {
			return out, nil
		}
		var last snowflake.ID
		for _, m := range res.Messages {
			last = m.ID
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
		// Fewer than a page can still mean more (the docs say so for cold
		// history), so only an empty page ends the walk.
		q.offset += searchPageSize
		if q.offset > maxOffset {
			q.offset = 0
			q.minID = last
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
	}
}

// followUps replays bot.chainContinuation over the channel history after a
// ping: the author's next messages that carry media but no trigger of their
// own, each within bot.ChainWindow of the last one that joined, up to `budget`
// of them. Other people's messages in between are ignored, as the live chain
// ignores them (it is keyed per author). Stops early at the author's next ping
// or bot mention — that message is handled by its own rule — and at the launch
// day.
//
// Replies to the set (inSet) are skipped rather than counted: the reply rule
// owns those, and in the live bot they neither spend a slot nor move the clock.
func (d *discordREST) followUps(ctx context.Context, hit discord.Message, inSet map[snowflake.ID]bool, budget int) ([]discord.Message, error) {
	if budget <= 0 {
		return nil, nil
	}
	const page = 50
	var out []discord.Message
	lastAt := hit.CreatedAt
	after := hit.ID
	for {
		var msgs []discord.Message
		err := withRetry(ctx, func() error {
			var err error
			msgs, err = d.rest.GetMessages(hit.ChannelID, 0, 0, after, page, rest.WithCtx(ctx))
			return err
		})
		if err != nil {
			return nil, err
		}
		if len(msgs) == 0 {
			return out, nil
		}
		// Discord hands `after` pages back newest first; the rule reads forward.
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
		for _, m := range msgs {
			after = m.ID
			if m.CreatedAt.Sub(lastAt) > bot.ChainWindow || !m.CreatedAt.Before(d.ceiling) {
				return out, nil
			}
			if m.Author.ID != hit.Author.ID || inSet[m.ID] {
				continue
			}
			if len(m.MentionRoles) > 0 || bot.BotMentionedIn(m.Content, d.appID.String()) {
				return out, nil
			}
			if m.MessageReference != nil && m.MessageReference.MessageID != nil && inSet[*m.MessageReference.MessageID] {
				continue
			}
			if len(bot.CollectMedia(m)) == 0 {
				continue
			}
			out = append(out, m)
			lastAt = m.CreatedAt
			if len(out) >= budget {
				return out, nil
			}
		}
		if len(msgs) < page {
			return out, nil
		}
	}
}

// parseWindowTime reads a -after/-before value: a day (YYMMDD, midnight UTC) or
// a full RFC3339 timestamp for a window that starts or ends mid-day.
func parseWindowTime(s string) (time.Time, error) {
	if t, err := time.Parse("060102", s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("want YYMMDD or an RFC3339 timestamp (2026-09-28T10:00:00Z), got %q", s)
	}
	return t.UTC(), nil
}

// scanChannel walks each channel's history oldest first across (minID, maxID)
// and returns every message in it. Used to fill a stretch the live bot was down
// for, where there is no single role to search on: the search endpoint has no
// "mentions any role" filter, and the bot's other triggers (a bare follow-up,
// an idol named in the text) cannot be searched for at all.
func (d *discordREST) scanChannel(ctx context.Context, channels []snowflake.ID, minID, maxID snowflake.ID) ([]discord.Message, error) {
	const page = 100
	var out []discord.Message
	for _, ch := range channels {
		after, scanned := minID, 0
		for {
			var msgs []discord.Message
			err := withRetry(ctx, func() error {
				var err error
				msgs, err = d.rest.GetMessages(ch, 0, 0, after, page, rest.WithCtx(ctx))
				return err
			})
			if err != nil {
				return nil, err
			}
			if len(msgs) == 0 {
				break
			}
			// Discord hands `after` pages back newest first.
			sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
			past := false
			for _, m := range msgs {
				after = m.ID
				if m.ID >= maxID {
					past = true
					break
				}
				scanned++
				out = append(out, m)
			}
			if past || len(msgs) < page {
				break
			}
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
		}
		log.Printf("   read %d message(s) in channel %s", scanned, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// repliesTo finds the replies to each hit that the live bot's reply trigger
// would have folded into the hit's set: same author, in the same channel. It
// follows replies-to-replies for a few rounds, since a drop posted as a chain
// of replies is common.
func (d *discordREST) repliesTo(ctx context.Context, hits []discord.Message) (map[snowflake.ID][]discord.Message, error) {
	// Discord accepts up to 100 ids per query, but its front end caps the
	// request line at 4094 bytes and each id costs ~42 of them; 75 leaves room
	// for the rest of the query.
	const batch, rounds = 75, 3

	// root of every message that belongs to a set, so a reply to a reply is
	// attributed to the original hit.
	rootOf := map[snowflake.ID]snowflake.ID{}
	authorOf := map[snowflake.ID]snowflake.ID{}
	for _, h := range hits {
		rootOf[h.ID] = h.ID
		authorOf[h.ID] = h.Author.ID
	}
	out := map[snowflake.ID][]discord.Message{}
	frontier := make([]snowflake.ID, 0, len(hits))
	for _, h := range hits {
		frontier = append(frontier, h.ID)
	}

	for round := 0; round < rounds && len(frontier) > 0; round++ {
		var next []snowflake.ID
		for start := 0; start < len(frontier); start += batch {
			end := min(start+batch, len(frontier))
			msgs, err := d.searchAll(ctx, searchQuery{
				repliedToMessageIDs: frontier[start:end],
				channelIDs:          channelIDs,
			})
			if err != nil {
				return nil, err
			}
			for _, m := range msgs {
				if m.MessageReference == nil || m.MessageReference.MessageID == nil {
					continue
				}
				parent := *m.MessageReference.MessageID
				root, ok := rootOf[parent]
				if !ok || authorOf[root] != m.Author.ID {
					continue // not one of ours, or somebody else replying
				}
				if _, already := rootOf[m.ID]; already {
					continue // a hit itself, or seen in an earlier round
				}
				rootOf[m.ID] = root
				out[root] = append(out[root], m)
				next = append(next, m.ID)
			}
		}
		frontier = next
	}
	return out, nil
}
