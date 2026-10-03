# Discord Submission Pack — Message Content Intent Review

Why this file exists: Discord notified that the app crossed the
**10,000 reachable users** threshold, which starts a **90-day window** to apply
for the Message Content privileged intent (since June 2026 this review is
separate from the 100-server app verification; the prep below covers both).
<!-- TODO(operator): write the exact deadline date from the notice here: ____ -->
Miss the window → the intent is switched off and every passive ingestion
trigger (role ping, reply, follow-up, text detection) silently stops.
Once granted, **reapplication is annual** — keep this file current so next
year's renewal is a copy-paste.

## Portal checklist (do before submitting)

- [ ] App is owned by a **Team** (not a personal account); every team member
      has verified email + MFA; team owner completes **Stripe identity
      verification** (needed for the 100-server verification anyway).
- [ ] **Privacy Policy URL:** `https://goyangi.pics/privacy` — must return real
      content to a plain fetch (it does: the route is SSR).
- [ ] **Terms of Service URL:** `https://goyangi.pics/terms`.
- [ ] App description in the portal matches the one below (reviewers check that
      stated functionality covers everything the bot does — ToS §6 forbids
      changing scope later without re-submitting).
- [ ] Privileged intents: **only Message Content toggled ON**. Presence and
      Server Members stay OFF (the gateway code requests
      `Guilds | GuildMessages | MessageContent` and nothing else — `bot/bot.go`).
- [ ] Screenshots/video ready (see last section).
- [ ] Pinned ingestion notice posted in every `DISCORD_ALLOWED_CHANNEL_IDS`
      channel (text below) — reviewers respond well to visible user notice.

## App description (paste into the portal)

> Goyangi archives K-pop media posted in specific, admin-allowlisted channels
> of one community server to the public fan-gallery goyangi.pics, credited to
> the poster's Discord username. Posters trigger archiving by pinging idol
> roles or mentioning the bot; moderators can archive a named message with a
> command. The bot also posts "new upload" announcements and provides lookup
> commands (/show, /match, /random, /top, /source, /privacy). A pinned notice
> in each archive channel explains the archiving and links the privacy policy;
> /privacy provides the policy, deletion and opt-out routes in-app.

## Use case for Message Content (the core answer)

> The bot archives media drops from allowlisted channels of one community.
> Its triggers are, by design, messages that never mention or invoke the bot:
> a poster pings an idol role (e.g. `@Wonyoung [IVE]`) with attachments; a
> poster replies to their own earlier drop to extend it; a poster continues a
> drop across several plain messages; or a message names an idol in plain text
> with media attached. To see the content and attachments of those messages at
> all, the bot needs the Message Content intent — without it Discord delivers
> them empty.
>
> Why interactions can't replace it: the posting flow belongs to the
> community's existing habits — people post drops as normal messages in
> archive channels; they do not run commands. Mention-triggered delivery only
> covers messages that @mention the bot, which is the rare explicit case (and
> for single named messages we already use a message context-menu command,
> which works without the intent). The passive triggers are the product.
>
> Scope minimisation: passive processing is hard-confined to an allowlist of
> channel IDs — the allowlist check is the first thing that runs, before any
> parse, log, or side effect; messages anywhere else are dropped with zero
> processing. The gateway subscription is Guilds + GuildMessages +
> MessageContent only. DMs are ignored.

## Data-handling answers

**What is stored, and where:**

- Media files from archived posts → Cloudflare R2 (EU region), public CDN.
- Poster's Discord username → public credit on the archived content
  (`uploaders` collection).
- A jump link to the source message (guild/channel/message id) → provenance
  field on the content record.

**What is NOT stored:**

- Message text — parsed transiently for `idol:`/`group:`/`tags:` lines and
  idol-name detection, then discarded. Error logs record the parse error and
  jump link only, never the message body.
- Author IDs — held in memory (1-hour TTL) solely to group a poster's
  consecutive messages into one set; never written to the database or logs.
- Nothing from non-allowlisted channels, DMs, presence, or member lists.

**Retention:** archived content stays until removed (that is the service);
operational logs are pruned at 90 days by a daily job; request logs at most
7 days. **Deletion route:** the `/privacy` command, the takedown page
(`https://goyangi.pics/takedown`) and goyangi.pics@protonmail.com — any poster
can have their credit anonymized, their content removed, and future ingestion
of their posts blocked. Account holders have self-service deletion and JSON
export on their profile page.

**Sharing:** none. No data brokers, ad networks, analytics, or monetization of
API data; data is never used to train ML/AI models. Cloudflare acts as a
storage processor under its standard DPA.

**Security:** HTTPS everywhere; hashed passwords; database and media
encrypted at rest via host-level disk encryption
<!-- TODO(operator): confirm the host disk is actually encrypted; PB_ENCRYPTION_KEY
only encrypts the settings table, so do not cite it as at-rest encryption -->;
admin access limited to the single operator with MFA; bot token and secrets in
environment config, not in the repo; SSRF-hardened outbound fetching.

## Pinned notice for archive channels (paste into Discord)

> 📸 Media posted in this channel may be automatically archived to
> https://goyangi.pics and publicly credited to your Discord username.
> Details: https://goyangi.pics/privacy (or run /privacy). Opt out or request
> removal: goyangi.pics@protonmail.com

## Screenshots / video to attach

Reviewers explicitly ask for the feature in action. Record one short clip (or
screenshots) showing:

1. A role-ping drop in an allowlisted channel → bot reacts → content appears
   on goyangi.pics credited to the poster.
2. A reply/follow-up joining the same set.
3. The same message posted in a NON-allowlisted channel being ignored
   (demonstrates the allowlist).
4. `/privacy` output and the pinned notice.

## Known review pressure points (pre-empted)

- *"Could a context-menu command do this?"* — it exists (`Ingest this
  message`) and is used for one-offs; the passive triggers are the product,
  see the use-case answer.
- *Relay/anonymity apps:* posts made through relay apps are credited to the
  invoking user read from the interaction metadata Discord attaches to the
  relayed message. Disclose if asked — it is attribution of the poster, not
  deanonymization of a third party.
- *Global commands, one-guild allowlist:* commands are registered globally,
  but every passive trigger is confined to the configured allowlist and the
  manual paths are role-gated per guild (`DISCORD_INGEST_ROLE_IDS` — must be
  set in prod; empty disables the role gate).
