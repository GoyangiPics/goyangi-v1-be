// Command recreditrelay hands the posts a relay app was credited with back to
// the person its "Posted by:" line names.
//
// Relay apps post on people's behalf and, when the person isn't anonymous, say
// who in a credit line. Until eff8832 the bot read that line only as plain text
// at the start of the message's content, so an app that wrote it as small text
// (`-# Posted by: kuro🍊`), in an embed, in Components V2 text or in a forwarded
// snapshot had its posts credited to the app instead. The live parser is fixed;
// this repairs the posts from before.
//
// For every post credited to -uploader it reads the post's Discord message
// again (contents.discord — once per message, since a message's posts share it)
// and runs the live bot's own decision on it: bot.PostedByLookup and
// bot.FindUploaderIn are the two halves of creditFromPostedBy. Match-only, by
// name or alias, against the uploaders that exist; nobody is created. A line
// naming nobody here, or no line at all (a genuinely anonymous post), leaves
// the post with the app. See directory.decide for the rest.
//
// Only contents.uploader is written. A set's uploaders are derived from its
// posts by hooks/ownership.go (syncSet, after every post update), so the person
// joins each set their posts are in, and the app leaves it only once no post in
// it is still the app's. Nothing here writes contents_sets; after a commit the
// touched sets are read back to check the hook did its part.
//
// Dry run by default: it fetches, decides and reports, and writes nothing.
// -commit writes. Safe to rerun: each run starts from the posts still credited
// to the app, so a moved post is never looked at again, a failed write is
// retried, and a name added as an alias since the last run is picked up.
//
//	go run ./scripts/recreditrelay -url https://api.example.com -uploader geatafg19fxs4hq
//	go run ./scripts/recreditrelay -url https://api.example.com -uploader geatafg19fxs4hq -report recredit.tsv
//	go run ./scripts/recreditrelay -url https://api.example.com -uploader geatafg19fxs4hq -commit -limit 5
//
// Each write is a normal record update: it fires the update hooks and a
// realtime event, and bumps `updated`. Of the hooks, only syncSet has anything
// to do — no file URL changes, so the R2 cleanup finds nothing.
//
// Environment (a .env in the working directory is loaded):
//
//	DISCORD_TOKEN                        bot token (REST only — the live bot is undisturbed)
//	PB_ADMIN_EMAIL / PB_ADMIN_PASSWORD   superuser; needed only for -commit
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/joho/godotenv"
)

func main() {
	log.SetFlags(log.Ltime)
	if err := godotenv.Load(); err == nil {
		log.Printf("ℹ️  loaded .env")
	}

	var (
		baseURL     = flag.String("url", "http://127.0.0.1:8090", "PocketBase base URL")
		relayID     = flag.String("uploader", "", "uploader id of the relay app whose posts to re-credit (required)")
		email       = flag.String("email", os.Getenv("PB_ADMIN_EMAIL"), "superuser email (prefer PB_ADMIN_EMAIL)")
		password    = flag.String("password", os.Getenv("PB_ADMIN_PASSWORD"), "superuser password (prefer PB_ADMIN_PASSWORD)")
		commit      = flag.Bool("commit", false, "write the new credits (default: report only)")
		limit       = flag.Int("limit", 0, "stop after this many Discord messages, oldest first (0 = all)")
		concurrency = flag.Int("concurrency", 4, "Discord messages fetched in parallel")
		reportPath  = flag.String("report", "", "also write every post's decision to this TSV file")
	)
	flag.Parse()

	if *relayID == "" {
		log.Fatal("-uploader is required: the relay app's uploader id")
	}
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN is required")
	}
	if *commit && (*email == "" || *password == "") {
		log.Fatal("-commit needs superuser credentials: PB_ADMIN_EMAIL and PB_ADMIN_PASSWORD, or -email/-password")
	}
	if *concurrency < 1 {
		log.Fatal("-concurrency must be at least 1")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── PocketBase ──
	pb := newPBClient(*baseURL)
	if *email != "" && *password != "" {
		if err := pb.authenticate(*email, *password); err != nil {
			log.Fatalf("PocketBase auth failed: %v", err)
		}
		log.Printf("🔑 PocketBase: %s (superuser)", pb.baseURL)
	} else {
		log.Printf("🔓 PocketBase: %s (public reads; no credentials)", pb.baseURL)
	}
	relay, err := pb.getUploader(*relayID)
	if err != nil {
		log.Fatalf("uploader %s: %v", *relayID, err)
	}
	uploaders, err := pb.uploadersNewestFirst()
	if err != nil {
		log.Fatalf("uploaders: %v", err)
	}
	dir := newDirectory(uploaders)
	posts, err := pb.postsCreditedTo(relay.ID)
	if err != nil {
		log.Fatalf("posts: %v", err)
	}
	msgs, noLink := byMessage(posts)
	log.Printf("📋 %d post(s) credited to %q, from %d message(s); %d uploaders to match against",
		len(posts), relay.Name, len(msgs), len(uploaders))
	if *limit > 0 && len(msgs) > *limit {
		msgs = msgs[:*limit]
		noLink = nil
		log.Printf("⏹  -limit %d: only the oldest %d message(s)", *limit, len(msgs))
	}

	// ── Discord ──
	dc, botName, err := newDiscord(token)
	if err != nil {
		log.Fatalf("Discord: %v", err)
	}
	log.Printf("🤖 Discord as %s — fetching %d message(s)…", botName, len(msgs))
	started := time.Now()
	dc.fetchAll(ctx, msgs, *concurrency)
	if ctx.Err() != nil {
		log.Fatal("⏹  interrupted while fetching; nothing was written")
	}
	log.Printf("📬 fetched in %s", time.Since(started).Round(time.Second))

	// ── Decide ──
	results := decideAll(dir, relay.ID, msgs, noLink)
	plan := planSets(results, func(r result) bool { return r.outcome == moves })
	printSummary(relay, results, msgs, plan, *commit)
	if *reportPath != "" {
		if err := writeReport(*reportPath, results); err != nil {
			log.Fatalf("report: %v", err)
		}
		log.Printf("📝 every decision written to %s", *reportPath)
	}

	if !*commit {
		log.Printf("🔍 DRY RUN — nothing was written. Pass -commit to apply.")
		return
	}

	// ── Write ──
	moved := map[string]bool{}
	var todo []result
	for _, r := range results {
		if r.outcome == moves {
			todo = append(todo, r)
		}
	}
	failed, interrupted := 0, false
	for i, r := range todo {
		if ctx.Err() != nil {
			interrupted = true
			log.Printf("⏹  interrupted after %d of %d write(s)", i, len(todo))
			break
		}
		if err := pb.recredit(r.post.ID, r.target.ID); err != nil {
			failed++
			log.Printf("❌ %s → %s: %v", r.post.ID, r.target.Name, err)
			continue
		}
		moved[r.post.ID] = true
		if n := len(moved); n%250 == 0 {
			log.Printf("   … %d/%d written", n, len(todo))
		}
	}
	log.Printf("── wrote %d post(s), %d failed", len(moved), failed)

	verify(pb, relay, results, moved)
	if failed > 0 || interrupted {
		// Non-zero so a partial run is visible. Safe to leave: whatever didn't
		// move is still the app's, and a rerun picks up exactly that.
		os.Exit(1)
	}
}

// decideAll turns each message's fetch into a verdict for each of its posts.
func decideAll(dir *directory, relayID string, msgs []*message, noLink []post) []result {
	var results []result
	for _, p := range noLink {
		results = append(results, result{post: p, verdict: verdict{outcome: badLink}})
	}
	for _, m := range msgs {
		v := m.failure
		if m.msg != nil {
			v = dir.decide(*m.msg, relayID)
		}
		for _, p := range m.posts {
			results = append(results, result{post: p, verdict: v})
		}
	}
	return results
}

// verify reads back what the commit did: how many posts the app still has, and
// whether syncSet left each touched set listing the app exactly when one of its
// posts there is still the app's.
func verify(pb *pbClient, relay uploader, results []result, moved map[string]bool) {
	left, err := pb.postsCreditedTo(relay.ID)
	if err != nil {
		log.Printf("⚠️  could not recount the app's posts: %v", err)
	} else {
		log.Printf("📋 %d post(s) still credited to %q", len(left), relay.Name)
	}

	plan := planSets(results, func(r result) bool { return moved[r.post.ID] })
	listing, err := pb.setsListing(relay.ID)
	if err != nil {
		log.Printf("⚠️  could not read back the sets: %v", err)
		return
	}
	var wrong []string
	for id, stays := range plan.stays {
		if listing[id] != stays {
			wrong = append(wrong, id)
		}
	}
	if len(wrong) == 0 {
		log.Printf("✅ all %d touched set(s) list %q exactly when they still hold a post of the app's", plan.touched, relay.Name)
		return
	}
	// syncSet is best effort and logs its own failures on the server; saving any
	// post in the set again re-derives it.
	log.Printf("⚠️  %d touched set(s) disagree with their posts about %q:", len(wrong), relay.Name)
	for _, id := range wrong {
		log.Printf("   %s (should list the app: %v)", id, plan.stays[id])
	}
}

// ─── Discord ─────────────────────────────────────────────────────────────────

// message is one Discord message and the posts it became.
type message struct {
	link  string
	ref   messageRef
	posts []post

	msg     *discord.Message // set once fetched
	failure verdict          // why not, when msg is nil
}

// byMessage groups posts by the message their jump link points at, in the
// order the messages first appear, and sets aside the posts with no usable
// link. Keyed by message id rather than the link's text, so two spellings of
// one link are still fetched once.
func byMessage(posts []post) (msgs []*message, noLink []post) {
	byID := map[string]*message{}
	for _, p := range posts {
		ref, ok := parseJumpLink(p.Discord)
		if !ok {
			noLink = append(noLink, p)
			continue
		}
		m := byID[ref.message.String()]
		if m == nil {
			m = &message{link: p.Discord, ref: ref}
			byID[ref.message.String()] = m
			msgs = append(msgs, m)
		}
		m.posts = append(m.posts, p)
	}
	return msgs, noLink
}

type discordREST struct {
	rest rest.Rest
}

func newDiscord(token string) (*discordREST, string, error) {
	r := rest.New(rest.NewClient(token))
	app, err := r.GetCurrentApplication()
	if err != nil {
		return nil, "", fmt.Errorf("who am I: %w", err)
	}
	return &discordREST{rest: r}, app.Name, nil
}

// fetchAll reads every message, a few at a time. disgo's client waits out the
// rate limits itself; each worker writes only to the messages it took.
func (d *discordREST) fetchAll(ctx context.Context, msgs []*message, workers int) {
	jobs := make(chan *message)
	var wg sync.WaitGroup
	var done atomic.Int64
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range jobs {
				d.fetch(ctx, m)
				if n := done.Add(1); n%250 == 0 {
					log.Printf("   … %d/%d messages", n, len(msgs))
				}
			}
		}()
	}
feed:
	for _, m := range msgs {
		select {
		case jobs <- m:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
}

func (d *discordREST) fetch(ctx context.Context, m *message) {
	var msg *discord.Message
	err := withRetry(ctx, func() error {
		var err error
		msg, err = d.rest.GetMessage(m.ref.channel, m.ref.message, rest.WithCtx(ctx))
		return err
	})
	switch {
	case err == nil:
		m.msg = msg
	case isNotFound(err):
		m.failure = verdict{outcome: notFound}
	default:
		m.failure = verdict{outcome: unreadable, err: err.Error()}
	}
}

// isNotFound is a deleted message, or a deleted channel.
func isNotFound(err error) bool {
	var restErr *rest.Error
	return errors.As(err, &restErr) && restErr.Response != nil && restErr.Response.StatusCode == http.StatusNotFound
}

// withRetry runs a Discord REST call, retrying a handful of times on a 5xx, as
// scripts/backfilldiscord does. Anything else (4xx, context cancelled) returns
// at once; disgo has already waited out any 429.
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

// ─── Report ──────────────────────────────────────────────────────────────────

func printSummary(relay uploader, results []result, msgs []*message, plan setPlan, commit bool) {
	would := "would move"
	if commit {
		would = "to move"
	}
	fetched := 0
	for _, m := range msgs {
		if m.msg != nil {
			fetched++
		}
	}
	byOutcome := countOutcomes(results)
	targets := tallyBy(results, func(r result) (string, string, bool) {
		if r.outcome != moves {
			return "", "", false
		}
		return r.target.ID, r.target.Name, true
	})

	log.Printf("── summary for %q (%s)", relay.Name, relay.ID)
	log.Printf("   posts on the app:     %d, from %d message(s)", len(results), len(msgs))
	log.Printf("   messages fetched:     %d of %d", fetched, len(msgs))
	m := byOutcome[moves]
	log.Printf("   ✅ %s:         %d post(s) from %d message(s), to %d uploader(s)", would, m.posts, m.messages, len(targets))
	log.Printf("   stay with the app:    %d post(s)", len(results)-m.posts)
	for _, o := range outcomes[1:] {
		if t := byOutcome[o]; t.posts > 0 {
			log.Printf("      %5d post(s) / %4d message(s)  %s", t.posts, t.messages, o.describe())
		}
	}
	log.Printf("   sets touched:         %d — the app leaves %d, stays a co-uploader on %d; %d end up shared by several people",
		plan.touched, plan.appLeaves, plan.appStays, plan.shared)

	if len(targets) > 0 {
		log.Printf("── %s, by uploader (posts / messages)", would)
		for _, t := range targets {
			log.Printf("   %5d %4d  %s", t.posts, t.messages, t.name)
		}
	}
	printTallies(`"Posted by:" names that match no uploader (posts / messages) — add one as an alias and rerun`,
		tallyBy(results, func(r result) (string, string, bool) {
			return r.posted, fmt.Sprintf("%q", r.posted), r.outcome == unmatched
		}))
	printTallies("names someone blocked or opted out of Discord import (posts / messages) — left with the app",
		tallyBy(results, func(r result) (string, string, bool) {
			if r.outcome != optedOut {
				return "", "", false
			}
			return r.target.ID, fmt.Sprintf("%s (%q)", r.target.Name, r.posted), true
		}))
	printLinks("messages not found (404)", uniqueLinks(results, notFound))
	printLinks("messages that could not be read", uniqueLinks(results, unreadable))
	printLinks("not posted by an app or webhook", uniqueLinks(results, notRelayed))
	printLinks("Discord names who ran the app's command — check by hand", uniqueLinks(results, viaInteraction))
}

func printTallies(title string, ts []tally) {
	if len(ts) == 0 {
		return
	}
	log.Printf("── %s", title)
	for _, t := range ts {
		log.Printf("   %5d %4d  %s", t.posts, t.messages, t.name)
	}
}

func printLinks(title string, links []string) {
	if len(links) == 0 {
		return
	}
	log.Printf("── %s: %d", title, len(links))
	for _, l := range links {
		log.Printf("   %s", l)
	}
}

// uniqueLinks is the distinct jump links of the posts with outcome o, each with
// Discord's error where there was one.
func uniqueLinks(results []result, o outcome) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range results {
		if r.outcome != o || seen[r.post.Discord] {
			continue
		}
		seen[r.post.Discord] = true
		line := r.post.Discord
		if o == badLink {
			line = fmt.Sprintf("%s (post %s)", r.post.Discord, r.post.ID)
		}
		if r.err != "" {
			line += " — " + r.err
		}
		out = append(out, line)
	}
	return out
}

// writeReport is every post and its decision, one per line.
func writeReport(path string, results []result) error {
	var b strings.Builder
	b.WriteString("post\tset\tdiscord\toutcome\tposted_by\tuploader_id\tuploader_name\terror\n")
	for _, r := range results {
		var id, name string
		if r.target != nil {
			id, name = r.target.ID, r.target.Name
		}
		fields := []string{r.post.ID, r.post.Set, r.post.Discord, r.outcome.String(), r.posted, id, name, r.err}
		for i, f := range fields {
			fields[i] = strings.NewReplacer("\t", " ", "\n", " ").Replace(f)
		}
		b.WriteString(strings.Join(fields, "\t") + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
