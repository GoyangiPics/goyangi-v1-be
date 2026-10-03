package bot

import (
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/disgoorg/snowflake/v2"
)

// Configuration sourced from the environment (with fallbacks to the original
// hardcoded values so existing deployments keep working). Override per-deploy.

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// csvSnowflakeEnv parses a comma-separated list of Discord ids from the
// environment. Invalid entries are logged and skipped rather than crashing
// the boot.
func csvSnowflakeEnv(key, fallback string) []snowflake.ID {
	parts := strings.Split(envOr(key, fallback), ",")
	out := make([]snowflake.ID, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := snowflake.Parse(p)
		if err != nil {
			slog.Error("invalid Discord id in env", "key", key, "value", p)
			continue
		}
		out = append(out, id)
	}
	return out
}

// publicBaseURL is the public site URL used to build shareable links.
var publicBaseURL = strings.TrimRight(envOr("GOYANGI_PUBLIC_URL", "https://goyangi.pics"), "/")

// supportEmail is the contact for data-deletion and ingestion opt-out
// requests, surfaced by /privacy. Keep in sync with the frontend's
// NUXT_PUBLIC_SUPPORT_EMAIL.
var supportEmail = envOr("GOYANGI_SUPPORT_EMAIL", "goyangi.pics@protonmail.com")

// uploadPostChannelID is where site uploads are announced (autopost.go).
// Returns 0 when unset or unparseable, which skips posting rather than
// guessing at a channel.
//
// Like DISCORD_EMOJI_SUCCESS, the fallback is the MAIN bot's channel, so the
// dev deploy MUST override DISCORD_POST_CHANNEL_ID or it will announce dev
// uploads in the production channel.
var uploadPostChannelID = sync.OnceValue(func() snowflake.ID {
	return snowflakeEnv("DISCORD_POST_CHANNEL_ID", "1530277196453773312")
})

// notifyChannelID is where "preview ready" notifications are posted
// (notify.go).
//
// Follows the same production-default convention as uploadPostChannelID above,
// which means the dev deploy MUST override DISCORD_NOTIFY_CHANNEL_ID or its
// preview notifications appear in the main bot's channel.
var notifyChannelID = sync.OnceValue(func() snowflake.ID {
	return snowflakeEnv("DISCORD_NOTIFY_CHANNEL_ID", "1530270176199839864")
})

// postGuildID is the guild whose roles the automatic pings are resolved
// against. Both bots live in the same guild, so main and dev share a default.
var postGuildID = sync.OnceValue(func() snowflake.ID {
	return snowflakeEnv("DISCORD_GUILD_ID", "1530268583983186031")
})

// snowflakeEnv reads a single Discord id from the environment, returning 0 when
// it is missing or malformed. Callers treat 0 as "feature not configured".
//
// Read lazily by the vars above. .env is now loaded before package init (the
// godotenv/autoload import in main.go), so eager reads would work too; lazy
// keeps them correct even if that import is ever dropped.
func snowflakeEnv(key, fallback string) snowflake.ID {
	raw := strings.TrimSpace(envOr(key, fallback))
	id, err := snowflake.Parse(raw)
	if err != nil {
		slog.Error("invalid Discord id in env", "key", key, "value", raw)
		return 0
	}
	return id
}

// Reaction emoji — the only in-channel feedback the bot still gives, now that
// outcome detail goes to the "system_logs" collection instead of a reply.
//
// Custom emoji use the reaction-endpoint form "name:id" WITHOUT angle brackets
// (`<:name:id>` is the message-content form and is rejected here). The name is
// cosmetic; only the id resolves, so renaming the emoji in Discord is safe.
//
// The success marker is an application-owned emoji and differs per bot, so the
// dev deploy MUST override DISCORD_EMOJI_SUCCESS — the fallback is the main
// bot's, following the same production-default convention as the values above.
var (
	emojiSuccess = envOr("DISCORD_EMOJI_SUCCESS", "goyangi_success:1530520100409573447")
	// Neither of these is reacted with any more, and both are kept only to say
	// so: ❌ reads as "the bot broke" and ⚠️ as a public correction, when in both
	// cases the thing that needs acting on is a system_logs entry, not a mark on
	// somebody's message. DISCORD_EMOJI_WARNING / DISCORD_EMOJI_FAILURE are
	// therefore inert.
	emojiFailure = envOr("DISCORD_EMOJI_FAILURE", "❌")
	// emojiSkipped marks a message we deliberately did not ingest. Distinct from
	// the two above: it is not a complaint, it is the only signal a blocked
	// poster gets that the silence is deliberate.
	emojiSkipped = envOr("DISCORD_EMOJI_SKIPPED", "⏭️")
)

// ownMediaHosts are hosts serving content we already store. Media links
// pointing at them are skipped instead of being downloaded and re-uploaded, so
// reposting a goyangi link into a scraping channel can't duplicate the record
// (or, for a mirrored item, loop it back through the encode pipeline).
//
// Lazy as belt-and-braces: .env is loaded before package init (the
// godotenv/autoload import in main.go), but a lazy read stays correct even if
// that import is ever dropped. The two production hosts are always included, so
// a dev deploy pointed at dev-cdn still skips prod links.
var ownMediaHosts = sync.OnceValue(func() map[string]bool {
	hosts := map[string]bool{}
	for _, raw := range []string{
		"https://cdn.goyangi.pics",
		"https://goyangi.pics",
		os.Getenv("R2_PUBLIC_URL"),
		os.Getenv("GOYANGI_PUBLIC_URL"),
	} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			slog.Warn("cannot parse own-media host", "value", raw)
			continue
		}
		hosts[strings.ToLower(u.Hostname())] = true
	}
	return hosts
})

// allowedChannelIDs is the set of channels the PASSIVE ingestion triggers are
// permitted in, built once from DISCORD_ALLOWED_CHANNEL_IDS. Those triggers fire
// on messages nobody pointed at the bot, so this is what decides where it may
// watch.
//
// It does NOT gate the explicit paths — /reupload and the "Ingest this message"
// context menu. There is nothing to confine when a person named one message and
// asked for it; those are gated on manualIngestRoleIDs instead.
var allowedChannelIDs = func() map[snowflake.ID]bool {
	m := map[snowflake.ID]bool{}
	for _, id := range csvSnowflakeEnv("DISCORD_ALLOWED_CHANNEL_IDS", "124767749099618304,1530276704801656954,1530277196453773312") {
		m[id] = true
	}
	return m
}()

// manualIngestRoleIDs restricts who can run the ingestion paths that are NOT
// confined to allowedChannelIDs: /reupload and the "Ingest this message"
// context menu. Both are explicit, deliberate actions on a named message, which
// is why neither is tied to a channel.
//
// That freedom is exactly what needs a gate. Without one, any guild member could
// make the bot download from any channel it can read and publish that to the
// site. DefaultMemberPermissions on each command hides it from regular members,
// but that is only a client-side hint in some clients, so this is the real
// check.
//
// Empty (the default) means role-gating is OFF and both fall back to the
// permission hint alone. Set it in production.
//
// DISCORD_REUPLOAD_ROLE_IDS is the original name, still honoured: this gated
// only /reupload until the context menu stopped being channel-confined, and an
// existing deployment should not need an env change to keep working.
var manualIngestRoleIDs = manualIngestRoles()

// Split out from the var so the legacy-name fallback is testable — that is the
// half of this that an existing deployment depends on.
func manualIngestRoles() map[snowflake.ID]bool {
	m := map[snowflake.ID]bool{}
	for _, id := range csvSnowflakeEnv("DISCORD_INGEST_ROLE_IDS", envOr("DISCORD_REUPLOAD_ROLE_IDS", "")) {
		m[id] = true
	}
	return m
}

// imgurWebClientID is the client id imgur's own web app sends with every API
// request — it is in the site's public JavaScript, and it is what the album
// pages themselves use to load their items. The fallback when IMGUR_CLIENT_ID
// is unset, because imgur has closed API registration and there is no other
// way to obtain one. It is imgur's, not ours: if it stops working, albums fall
// back to the OpenGraph path and ingest nothing, the pre-2026 behaviour.
const imgurWebClientID = "546c25a59c58ad7"

// imgurClientID is the Imgur API client id used to resolve album and gallery
// links (imgur_album.go): IMGUR_CLIENT_ID if set, else imgurWebClientID. Set
// it to "off" to disable the API path entirely.
//
// Lazy for the same reason as ownMediaHosts: package-level init runs before
// main() loads .env.
var imgurClientID = sync.OnceValue(func() string {
	switch v := strings.TrimSpace(os.Getenv("IMGUR_CLIENT_ID")); v {
	case "":
		return imgurWebClientID
	case "off":
		return ""
	default:
		return v
	}
})
