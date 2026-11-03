package main

import (
	"cmp"
	"net/url"
	"slices"
	"strings"

	"goyangi-v1-be/bot"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// The decisions, kept free of the network so they can be tested: where a post's
// message is, what the live bot would credit it to, and what the moves do to
// the sets they touch.

// post is one record credited to the relay app.
type post struct {
	ID      string `json:"id"`
	Set     string `json:"set"`
	Discord string `json:"discord"`
}

// uploader is one uploaders record.
type uploader struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Aliases           string `json:"aliases"`
	BlockIngest       bool   `json:"blockIngest"`
	SkipDiscordImport bool   `json:"skipDiscordImport"`
}

// messageRef is where a jump link points.
type messageRef struct {
	guild, channel, message snowflake.ID
}

// parseJumpLink reads contents.discord, which the bot writes as
// https://discord.com/channels/<guild>/<channel>/<message> (stampMessage).
// The older and client-specific hosts are accepted too; anything else, a DM
// link (@me) included, is not a message this script can look up.
func parseJumpLink(link string) (messageRef, bool) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return messageRef{}, false
	}
	switch strings.ToLower(u.Host) {
	case "discord.com", "www.discord.com", "discordapp.com", "www.discordapp.com",
		"ptb.discord.com", "canary.discord.com":
	default:
		return messageRef{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "channels" {
		return messageRef{}, false
	}
	var ids [3]snowflake.ID
	for i, s := range parts[1:] {
		id, err := snowflake.Parse(s)
		if err != nil || id == 0 {
			return messageRef{}, false
		}
		ids[i] = id
	}
	return messageRef{guild: ids[0], channel: ids[1], message: ids[2]}, true
}

// outcome is what happens to a message's posts.
type outcome int

const (
	moves          outcome = iota // the line names an existing uploader
	anonymous                     // no "Posted by:" line: the app's, as it should be
	unmatched                     // a line naming nobody here
	optedOut                      // names someone blocked, or who turned off Discord import
	namesRelay                    // the line resolves to the relay itself
	viaInteraction                // Discord names who ran the app's command
	notRelayed                    // not posted by an app or webhook
	notFound                      // the message is gone (404)
	unreadable                    // any other error fetching it
	badLink                       // no usable jump link on the post
)

// outcomes in the order the summary lists them.
var outcomes = []outcome{moves, anonymous, unmatched, optedOut, namesRelay, viaInteraction, notRelayed, notFound, unreadable, badLink}

func (o outcome) String() string {
	switch o {
	case moves:
		return "moves"
	case anonymous:
		return "anonymous"
	case unmatched:
		return "unmatched"
	case optedOut:
		return "opted-out"
	case namesRelay:
		return "names-relay"
	case viaInteraction:
		return "interaction"
	case notRelayed:
		return "not-relayed"
	case notFound:
		return "not-found"
	case unreadable:
		return "unreadable"
	case badLink:
		return "bad-link"
	}
	return "?"
}

// describe is the summary's line for each outcome that leaves posts with the app.
func (o outcome) describe() string {
	switch o {
	case anonymous:
		return `no "Posted by:" line (anonymous)`
	case unmatched:
		return "line names no existing uploader"
	case optedOut:
		return "names someone blocked or opted out of Discord import"
	case namesRelay:
		return "line names the app itself"
	case viaInteraction:
		return "Discord names who ran the command (live bot credits them, not the line)"
	case notRelayed:
		return "not posted by an app or webhook"
	case notFound:
		return "message not found (404)"
	case unreadable:
		return "message could not be read"
	case badLink:
		return "no usable jump link"
	}
	return o.String()
}

// verdict is the decision for one message, shared by all its posts.
type verdict struct {
	outcome outcome
	posted  string    // the credit line's name as written
	target  *uploader // who it resolved to (moves, optedOut)
	err     string    // unreadable: what Discord said
}

// directory is every uploader, newest first — the order findUploaderByNames
// reads them in, so a name two records share resolves to the same one here.
type directory struct {
	uploaders []uploader
	names     []bot.UploaderNames
}

func newDirectory(newestFirst []uploader) *directory {
	d := &directory{uploaders: newestFirst, names: make([]bot.UploaderNames, len(newestFirst))}
	for i, u := range newestFirst {
		d.names[i] = bot.UploaderNames{Name: u.Name, Aliases: u.Aliases}
	}
	return d
}

// decide is what the live bot's creditFor does with a relayed message today,
// restricted to what the posts' current credit can be wrong about.
//
// creditFor consults the "Posted by:" line only for a message an app or webhook
// posted and Discord names nobody behind; with interaction metadata it credits
// the person who ran the command instead, and a post credited to the app should
// not have that — so it is reported rather than guessed at. The line itself goes
// through the bot's own parser and matcher (bot.PostedByLookup,
// bot.FindUploaderIn): match-only, by name or alias.
//
// A match on someone blocked or opted out of Discord import is left with the
// app: the live bot would not have ingested the post for them at all, and
// putting it on their profile now is not what either flag asked for.
func (d *directory) decide(msg discord.Message, relayID string) verdict {
	if !msg.Author.Bot && msg.WebhookID == nil {
		return verdict{outcome: notRelayed}
	}
	if md := msg.InteractionMetadata; md != nil && md.User.Username != "" {
		return verdict{outcome: viaInteraction}
	}
	if in := msg.Interaction; in != nil && in.User.Username != "" {
		return verdict{outcome: viaInteraction}
	}

	posted, candidates := bot.PostedByLookup(msg)
	if posted == "" {
		return verdict{outcome: anonymous}
	}
	i := bot.FindUploaderIn(d.names, candidates...)
	if i < 0 {
		return verdict{outcome: unmatched, posted: posted}
	}
	target := &d.uploaders[i]
	switch {
	case target.ID == relayID:
		return verdict{outcome: namesRelay, posted: posted, target: target}
	case target.BlockIngest || target.SkipDiscordImport:
		return verdict{outcome: optedOut, posted: posted, target: target}
	}
	return verdict{outcome: moves, posted: posted, target: target}
}

// result is one post and its message's verdict.
type result struct {
	post post
	verdict
}

// tally counts posts and the messages they came from, under one name.
type tally struct {
	name            string
	posts, messages int
}

// tallyBy groups results by key, counting each message once. Sorted by posts,
// most first, then by name.
func tallyBy(results []result, keep func(result) (key, name string, ok bool)) []tally {
	byKey := map[string]*tally{}
	seen := map[string]bool{}
	for _, r := range results {
		key, name, ok := keep(r)
		if !ok {
			continue
		}
		t := byKey[key]
		if t == nil {
			t = &tally{name: name}
			byKey[key] = t
		}
		t.posts++
		if msgKey := key + "\x00" + r.post.Discord; !seen[msgKey] {
			seen[msgKey] = true
			t.messages++
		}
	}
	out := make([]tally, 0, len(byKey))
	for _, t := range byKey {
		out = append(out, *t)
	}
	slices.SortFunc(out, func(a, b tally) int {
		if c := cmp.Compare(b.posts, a.posts); c != 0 {
			return c
		}
		return cmp.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
	})
	return out
}

// countOutcomes is how many posts, and how many messages, came to each outcome.
// A message's posts share its verdict, so its link counts it once.
func countOutcomes(results []result) map[outcome]tally {
	out := map[outcome]tally{}
	seen := map[string]bool{}
	for _, r := range results {
		t := out[r.outcome]
		t.name = r.outcome.String()
		t.posts++
		if !seen[r.post.Discord] {
			seen[r.post.Discord] = true
			t.messages++
		}
		out[r.outcome] = t
	}
	return out
}

// setPlan is what the moves do to the sets they touch, as hooks/ownership.go's
// syncSet will derive it: a set's uploaders are the distinct uploaders of its
// posts, so the person joins as soon as one of theirs moves in, and the app
// stays while any post in the set is still the app's.
type setPlan struct {
	touched   int // sets with at least one post moving
	appLeaves int // every app post in it moves
	appStays  int // some stay: the app remains a co-uploader
	shared    int // the moves go to more than one person

	stays map[string]bool // touched set id → whether the app should still be on it
}

// planSets reads the moves off the results. moved says whether a post's move
// went through: during a dry run that is every post that would move, after a
// commit only the writes that succeeded.
func planSets(results []result, moved func(result) bool) setPlan {
	type setMoves struct {
		moving, staying int
		people          map[string]bool
	}
	sets := map[string]*setMoves{}
	for _, r := range results {
		if r.post.Set == "" {
			continue
		}
		s := sets[r.post.Set]
		if s == nil {
			s = &setMoves{people: map[string]bool{}}
			sets[r.post.Set] = s
		}
		if moved(r) {
			s.moving++
			s.people[r.target.ID] = true
		} else {
			s.staying++
		}
	}
	p := setPlan{stays: map[string]bool{}}
	for id, s := range sets {
		if s.moving == 0 {
			continue
		}
		p.touched++
		p.stays[id] = s.staying > 0
		if s.staying > 0 {
			p.appStays++
		} else {
			p.appLeaves++
		}
		if len(s.people) > 1 {
			p.shared++
		}
	}
	return p
}
