# goyangi v1 — backend

PocketBase (Go) backend for goyangi. Bundles three things in one process:

- **PocketBase** — database, auth, REST API, admin UI.
- **Media hooks** (`hooks/`) — on upload, transcode video/gif to AV1, encode an
  H.264 fallback, generate AVIF/WebP previews, optionally RIFE-interpolate
  gifs, and push everything to Cloudflare R2. Also exposes a stateless
  `POST /api/convert/avif` endpoint.
- **Discord bot** (`bot/`) — ingests media posted in allowed Discord channels
  into `contents`, and posts "AVIF ready" notifications.

## Requirements

- Go 1.25+
- `ffmpeg` / `ffprobe` on PATH, with libsvtav1, libx264, libwebp and aac
  (plus VAAPI or NVENC support for the GPU H.264 path)
- Optional: `avifenc` (libavif-tools) and `rife-ncnn-vulkan` for the avifenc
  and interpolation paths respectively
- Optional: a VAAPI-capable (Linux) or NVIDIA (NVENC) GPU for the H.264
  rendition (see below)
- A Cloudflare R2 bucket (credentials set in PocketBase's S3 storage settings)

## Stored renditions

Every video/gif upload produces up to four objects, keyed off one base name:

| Object | Field | What it is |
| --- | --- | --- |
| `<base>.mp4` | `original` | AV1, long edge ≤1920 — the canonical rendition |
| `<base>-sd.mp4` | `sd` | H.264 720p — compatibility fallback, boxed 1280×720 |
| `<base>.avif` / `.webp` | `preview` | animated preview |
| `<base>-static.*` | `static` | first-frame poster |

The AV1 original is capped on its **longer** edge, so portrait comes out
1080×1920 rather than crushed into a landscape box (it used to come out of the
1920×1080 box at ~608×1080). The SD rendition deliberately keeps the 1280×720
box — it is the budget rendition, and portrait staying small there is its size
budget at work. Like the still fix below the AV1 change is **forward-only** —
the source is deleted after encoding, so existing portrait videos stay at the
old size unless re-ingested.

Stills (`filetype: image`) produce three, and got the long-edge cap first — a
portrait pic coming out of the 1920×1080 box at ~608×1080 is what "pics look
low quality" was:

| Object | Field | What it is |
| --- | --- | --- |
| `<base>.avif` | `original` | AVIF, long edge ≤2560, crf 20 — the canonical still |
| `<base>-preview.avif` | `preview` | AVIF, long edge ≤1280, crf 30 — the grid rendition |
| `<base>-static.avif` | `static` | small poster, feeds OG/Discord embeds |

`original` and `preview` are distinct objects here. They used to be the same
key, which was harmless while the canonical still was itself small — but the
frontend loads `original` for image cards, so raising the cap without splitting
them would have made the masonry grid pull multi-megabyte files per tile.

A still is roughly 3–7× the bytes it used to be (most of that from the
resolution fix, the rest from crf 25→20). The pipeline deletes the uploaded
source, and there is no reprocess path, so this is **forward-only**: stills
already in the library stay at their original ~1080p cap.

An upload whose `filetype` is empty or unrecognised is now rejected with its
file left intact, so it can be corrected and reprocessed. It previously fell
through to the image branch, which reduced it to a single AVIF frame and deleted
the source.

The `sd` rendition exists because Safari never software-decodes AV1: Apple
devices below A17 Pro / M3 render an AV1 MP4 as a blank frame rather than
degrading. Clients should offer both to the browser and let it choose, rather
than picking a URL themselves:

```html
<video playsinline>
  <source src="…-sd.mp4" type='video/mp4; codecs="avc1.640028, mp4a.40.2"'>
  <source src="….mp4"    type='video/mp4; codecs="av01.0.08M.10"'>
</video>
```

All AV1 output is 10-bit, so one codecs string covers every video rendition:
`av01.0.08M.10`. Keep it accurate if the encoders ever change — an inaccurate
string makes a browser reject content it could actually have played.

Adding a rendition means touching four places: the encoder, the upload switch
in `moveFileToCustomR2Path`, **the `r2URLFields` list in
[hooks/r2_cleanup.go](hooks/r2_cleanup.go)** (miss it and the objects leak on
every delete *and* on every reprocess), and the collection schema.

### Object lifecycle

- **Upload** — every object `moveFileToCustomR2Path` puts in R2 is tracked, and
  rolled back if anything later in the function fails, including the final
  `app.Save`. Previously an upload followed by a failed save leaked silently.
- **Reprocess** — an update hook deletes objects the record has stopped pointing
  at. Keys are derived from metadata, so same-metadata reprocessing overwrites
  in place; keys change when `filetype` or `preview_format` is edited, or when
  idol/group/date change before a reprocess. Set
  `GOYANGI_R2_UPDATE_CLEANUP=0` to disable.
- **Delete** — one bounded queue, coalesced into batches, `MAX_R2_DELETE_JOBS`
  (default 8) concurrent deletes sharing a single filesystem client, drained on
  shutdown. Cascades go through the same path: `contents.set` is
  `cascadeDelete`, and PocketBase runs cascades through the full ORM, so
  deleting a 200-item set fires 200 child hooks. Each delete is skipped if
  another record still references the object, and skipped (not attempted) if the
  reference check itself fails — leaking bytes is recoverable, deleting live
  content is not.
- **Still leaks** — a hard crash between an R2 upload and its enqueue, or with
  keys still queued at shutdown. The only complete answer is a reconciliation
  sweep listing the bucket and deleting keys no `contents` row references. Not
  built; if you build it, start it log-only, because a bug in that sweep deletes
  live content, which is strictly worse than leaking bytes.

## GPU acceleration

The H.264 `sd` rendition encodes on the GPU (VAAPI or NVENC) so it overlaps the
CPU-bound AV1 encode instead of queueing behind it. Everything else is
deliberately CPU-only — ffmpeg's avif muxer mishandles hardware-encoded AV1
streams (libavif #2922), which is why the AVIF and AV1 paths stay on
libsvtav1.

On Fedora with an Intel Arc card:

```bash
sudo dnf install intel-media-driver libva-utils ffmpeg
vainfo | grep -i h264   # want VAEntrypointEncSlice on VAProfileH264High
```

On Windows or Linux with an NVIDIA card, a current driver and an ffmpeg build
with `h264_nvenc` (e.g. the gyan.dev full build) is all it takes.

`GOYANGI_H264_ENCODER` picks the encoder: `auto` (default) tries VAAPI, then
NVENC (a short test encode at boot), then libx264; `vaapi`, `nvenc` and `cpu`
force one. The server logs which path it picked at boot (`🎬 H.264: VAAPI
device …`, `🎬 H.264: NVENC …` or a warning that it fell back to libx264). Set
`GOYANGI_VAAPI_DEVICE` to point at a different render node, or to `off` to
force the CPU path.

A failed hardware encode falls back to libx264 for that file only — it is not
latched, so one pathological input can't quietly move the whole pipeline onto
the CPU. Repeated warnings in the log mean the driver, not the file.

## Setup

```bash
cp .env.example .env   # fill in DISCORD_TOKEN, R2_PUBLIC_URL, etc.
go run . serve
```

Admin UI: http://127.0.0.1:8090/_/ (or `--http=0.0.0.0:8080` as in the Dockerfile).

## Site uploads that are not announced

Site uploads are announced in `DISCORD_POST_CHANNEL_ID`, keyed by set so a
multi-file upload collapses to one message (`bot/autopost.go`). Two exceptions:

- Anything with a non-empty `discord` field — it came *from* Discord, so
  announcing it would echo the server back at itself.
- **Collection-mode uploads** (no set, at least one collection). These have no
  set to key on, so they would post once per file — twenty files, twenty
  role-pinging messages. Stickers are also setless but carry no collections, and
  are still announced.

## Authorisation that isn't an API rule

Almost everything is enforced by collection rules in `pb_schema.json`. One thing
can't be, and it's worth knowing why before someone tries to "simplify" it back:

**Adding content to a collection you don't own** is blocked by
`hooks/collections.go`, not by a rule. `contents.updateRule` deliberately leaves
`collections` out of its guard list — that omission is what lets anyone add
someone else's content to their own collection, which is the feature. Guarding
the other direction with a rule looks easy:

```
@request.body.collections.user.id = @request.auth.id
```

but it would pass every request the app actually sends. Membership is written
with PocketBase's relation modifiers (`collections+` / `collections-`), and
`RequestEvent.initRequestInfo` binds the **raw** body, so the key is literally
`collections+` and `@request.body.collections:isset` is false. The modifier key
can't be named either: the resolver only permits `[\w.:]` after
`@request.body.`, and `+` isn't a word character.

A hook runs after modifiers are resolved and sees the record's real final state,
whichever way it was written. Only additions are checked — see the file for why
removals stay open.

## Discord bot

> ⚠️ The bot requires the **Message Content** privileged intent. Enable it in
> the [Discord developer portal](https://discord.com/developers/applications)
> under *Bot → Privileged Gateway Intents*, or role-ping and reply ingestion
> receive empty message content/attachments and silently do nothing.

Media ingestion triggers (each item becomes a `contents` record, transcoded
and pushed to R2 exactly like a web-UI upload):

> The **passive** triggers below are confined to `DISCORD_ALLOWED_CHANNEL_IDS`:
> they fire on messages nobody pointed at the bot, so the allowlist is what
> decides where it may watch. They credit the message author.
>
> The **explicit** ones — `/reupload` and the **Ingest this message** context
> menu — work in any channel, because a person named one message and asked for
> it, and they credit that person. Being credited is what they are gated on:
> `DISCORD_INGEST_ROLE_IDS`, plus the caller's Discord name resolving to an
> `uploaders` record whose linked user account has `canUpload`.

- **Role ping** in an allowed channel (`DISCORD_ALLOWED_CHANNEL_IDS`): idol
  and group come from the pinged `Idol [Group]` role names.

  If the stated group doesn't contain that idol — `Eunbi [IZONE]` where the
  record is filed under Solo, `Yuju [GFRIEND]` likewise — the idol wins, as
  long as the name resolves to exactly one person in the whole directory. She
  and her real group are used, and the group that didn't resolve is still
  reported as unresolved so it can be added or aliased later. Ambiguous names
  are never guessed: if two idols share it, the stated group was the only thing
  that could have distinguished them.

  A role that parses to nothing at all (a group-only role like `@ifeye`) falls
  through to text detection rather than failing.
- **@-mention of the bot** (allowed channel): metadata comes from `key: value`
  lines in the message. Like every passive trigger it is confined to
  `DISCORD_ALLOWED_CHANNEL_IDS` (`prepareIngestion` checks the allowlist before
  anything else) — the only any-channel paths are the explicit ones above.
- **Reply** (allowed channel, same author) to a message that created a set:
  the new items join that set.
- **Message text** (allowed channel, last resort): a message with media that
  names an idol the directory knows, with no ping and no @-mention. For the
  cases pings don't cover — not every idol has a role, some people deliberately
  don't ping so as not to alert the channel, and some just forget.

  Strict on purpose, because it is the only rule that decides an attribution
  nobody stated: only `name` and the explicit `aliases` field are matched
  (nothing is inferred by splitting names up), matches must land on word
  boundaries, URLs are stripped first so a filename can't attribute a post, an
  idol is required, and a name shared by several groups is only accepted when
  one of those groups is named too.

  **Every hit is logged to `system_logs` as a warning**, with the strings that
  matched. That log is how the hit rate and the mistakes get reviewed — read it
  after a week and tune. `GOYANGI_DETECT_STOPWORDS` suppresses specific names
  (comma-separated) for directory entries that collide with ordinary English.
- **Bare follow-up** (allowed channel, same author): a message with media but no
  trigger of its own, posted shortly after that author's own ping, joins the set
  that ping created — no reply needed. Plenty of people post a drop as a run of
  separate messages, and before this only the first one was collected.

  Bounded by `chainMaxFollowUps` (3) and `chainWindow` (5 minutes from the last
  message that joined, not from the ping, so a slow drop stays one set). A new
  ping from the same author starts a fresh chain with a fresh budget, and a
  follow-up that pings a role or @-mentions the bot is handled by those rules
  instead.

  This is the one rule that infers intent rather than reading an explicit
  trigger, so it will sometimes be wrong — an unrelated clip posted right after
  a drop gets folded in. That is deliberate: a mod can split a set afterwards,
  but nothing can recover content that was never collected.

A message with **both** a role ping and an @-mention of the bot is deliberately
**skipped** and marked ⏭️ (`DISCORD_EMOJI_SKIPPED`). That is the "already
uploaded on the site by hand" signal: ping the roles so people see it, ping the
bot so it stays out. Either trigger on its own still ingests as above.

Media sources: Discord attachments, imgur links (normalized to the
`i.imgur.com/<id>.mp4` mirror form), imgur albums and galleries (every item,
via the post API, using imgur's own web client id unless `IMGUR_CLIENT_ID`
says otherwise — the page's OpenGraph tags, the old path, stopped existing in
2026),
`files.catbox.moe` links, `pixeldrain.com/u/<id>` links, and any direct media
URL (`mp4/webm/mov/webp/avif/jpg/png/gif`).

Optional `key: value` metadata lines (idol/group required if no roles pinged):

```
idol: Wonyoung, Yujin
group: IVE
tags: fancam, 4k
title: Love Dive stage
date: 240115          # or "today"; defaults to now
source: https://youtu.be/...   # falls back to first YouTube link in message
uploader: name        # defaults to the Discord username of the poster
filetype: sticker     # optional override (image/gif/video/sticker)
mirror: https://...   # attribution only — never ingested as an item
```

Discord-ingested items default to an **animated WebP** preview rather than AVIF
(`preview_format`), since they are seen mostly through Discord embeds and not
every client there decodes AVIF. Site uploads still choose per-upload.

An omitted `title:` is derived as `Idols - Groups`, which becomes the heading on
the content's page and in its social embed.

Multi-item messages create a `contents_sets` record; unknown uploaders and
tags are created on the fly, while idols/groups must already exist (the bot
replies with the names it couldn't resolve).

The reaction on the message says only whether anything was taken —
`DISCORD_EMOJI_SUCCESS` if it was, nothing at all if it wasn't, `⏭️` if the
skip was deliberate. A partial run reacts the same as a complete one: what
failed is a `system_logs` warning for whoever runs the bot, not a mark on the
poster's message. The reply still lists what didn't resolve.

Slash commands:

- `/post` — post a set to the current channel with role pings. `pings`
  autocompletes the server's roles and accumulates: pick one, type a comma,
  pick another. `mirror`/`source` are optional credits, `anonymous` drops the
  byline. Same layout as the automatic upload announcement below.
- `/reupload` — ingest the media in a linked message, credited to **whoever ran
  the command** rather than the message author. Takes a message jump link
  (right-click → Copy Message Link), one or more idols, and optional tags. For
  one-offs posted in social or group channels.

  `idols` and `tags` accumulate: each suggestion's value is everything already
  picked plus that entry, so choosing one appends rather than replaces, and the
  value carries a trailing comma to mark the last entry as committed. Discord
  caps an option value at 100 characters, which works out at roughly six idols.
  Groups are not asked for — every idol carries its own, so the chosen idols
  determine them.

  The content date is taken from the **linked message's** timestamp, not the time
  of the reupload, so a set lands under the date it was originally posted. A
  `date:` line in the target message still wins. `key: value` lines otherwise
  apply as usual, except that the explicit idol and tag options beat them.

  Ignores the channel allowlist, so it is gated three times:
  `DefaultMemberPermissions` (Manage Messages) hides it from regular members;
  `DISCORD_INGEST_ROLE_IDS` is the enforced role check, since the permission is
  only a client-side hint in some clients; and the caller's Discord display name
  or username must match an `uploaders` record (by `name` or `aliases`) whose
  `user` relation points at an account with `canUpload`. The same three gate
  **Ingest this message**. **Leaving `DISCORD_INGEST_ROLE_IDS` empty disables
  the role check** — the uploader link is always required. Set it in
  production. Reports its result ephemerally and reacts on the source message.
- `/revive` — re-encode the file behind an imgur link through the AVIF
  pipeline and get it back as an attachment. Works whether or not we ever
  stored the item; nothing enters the library.
- `/show` — post a piece of content with buttons for **AV1 HD**, **H.264 SD** and
  **Preview**. Each click answers privately, so two people can look at different
  renditions of the same post at once — which matters because Safari below
  A17 Pro / M3 renders an AV1 MP4 as a blank frame rather than degrading.

  The record id rides in the buttons' custom ids, so they survive a bot restart
  and never expire (unlike `/unwrap`'s pagination, whose page cursor has to live
  in memory).
- `/match` — find the Goyangi copy of a link (the AV1 MP4).
- `/unwrap` — paginated listing of a set/collection (`format`: mp4 / preview /
  static, `per_page` 1–5).
- `/source` — YouTube source for a piece of content.

`/match` and `/source` take a link in any shape an item can be referenced by:
an imgur mirror, a goyangi post link (`/single/<id>`), or a cdn file URL.
- `/convert` — convert an attachment or link through the encode pipeline
  (animated AVIF, MP4/AV1, sticker, or static thumbnail) and get the file
  back. Stateless — nothing enters the library.
- `/random` — a random piece of content. `idol` and `group` are optional and
  autocompleted from the library, `period` defaults to the last week, `count`
  returns 1–5 items, `format` picks embed (default — links the content page and
  lets Discord unfurl it), preview, or mp4.
- `/top` — the most-liked content, same options as `/random`.

Only a successful result is posted to the channel; errors and empty lookups
are ephemeral, visible to whoever ran the command.

Context menu (right-click a message → Apps): **Ingest this message** —
`/reupload` reached by right-clicking the message instead of pasting a link to
it. For posts from before the bot existed, and for channels where nobody pings
idol roles.

Picking it opens a **modal** asking for idols (required), tags and an optional
set link — the same three questions `/reupload` asks as options. A context-menu
command cannot declare options (Discord allows those only on slash commands), so
a modal is where they get asked. Any `key: value` lines already in the target
message prefill it. Modals have no autocomplete, so idols are typed by name;
unknown ones are reported back rather than guessed.

Works in **any channel**, not just `DISCORD_ALLOWED_CHANNEL_IDS`: nothing here
fires on its own, so there is no watching to confine. Carries `/reupload`'s
gates instead — `DefaultMemberPermissions` (Manage Messages),
`DISCORD_INGEST_ROLE_IDS`, and the caller's Discord name resolving to an
uploader whose linked account has `canUpload`. Credits **whoever ran it**, like
`/reupload` and unlike the passive triggers.

### Content posts

Both `/post` and the automatic upload announcement render the same layout, so
every content post in the server looks alike:

```
-# by nabi                                     ← omitted when anonymous
<@&IVE> <@&Leeseo [IVE]>                       ← role pings
[Mirror](https://i.imgur.com/abc.mp4)          ← masked: no embed of its own
[Source](https://youtu.be/xyz)
https://goyangi.pics/set/260727-ive-leeseo-…   ← bare: unfurls to the set embed
https://cdn.goyangi.pics/…/b.avif              ← up to 4 random previews
https://cdn.goyangi.pics/…/d.avif
```

The set link is bare so Discord unfurls it; Mirror and Source are masked
because a message gets at most five embeds and the previews should have them.
The previews are drawn at random from the set **excluding its first (newest)
item**, which is what the set page uses as its cover — so the embed and the
previews never show the same content.

Anything uploaded on the site is announced in `DISCORD_POST_CHANNEL_ID` once
its renditions are in R2, batched on a 30s debounce so a multi-file upload
posts once. Pings are resolved from the record's idols and groups against roles
named `Idol [Group]` (falling back to a bare `Idol` role) plus one per group;
names with no matching role are skipped. Items ingested *from* Discord are not
announced — they were already posted by hand in the channel they came from.

## Backfilling posts from before the bot

[scripts/backfilldiscord](scripts/backfilldiscord/main.go) archives the posts
that pinged one idol role before the bot launched (2026-07-24, a hard ceiling
in the script). It runs on any machine with ffmpeg: Discord over REST with the
bot token, PocketBase over HTTP as a superuser, R2 with its own credentials.
The server never encodes — records arrive with their rendition URLs already
set, which is the one shape the encode hook ignores — so nothing on it has to
change or restart, and none of the encode-time Discord notices fire.

It makes the same decisions the live role-ping trigger would have: same media
extractors, same metadata rules, same encoders (`hooks.EncodeRenditions`),
same-author replies, bare follow-ups and same-subject re-pings folded into
the set under the live chain rule's window and budget. Both `created` and `date` are the
message's timestamp — which the API refuses from anyone but a superuser, and
[hooks/provenance.go](hooks/provenance.go) exists to allow. Records are
stamped `origin: script`, `discord` with a handle on it: the whole backfill can
be reviewed (or removed) by that value, and once trusted
`-relabel script:discord -commit` turns it into plain `discord` through the
same hook. Imgur links are judged by attempting the download, never
by a page check — a link that 404s in a browser is often still downloadable,
and one that is really gone answers with imgur's `removed.png` placeholder,
which is dropped rather than archived.

Dry run by default; `-probe` also checks every link; `-commit` writes. Safe to
rerun: records carry their message's jump link, so a rerun plans only the items
of each message that have no finished record and redoes any half-written one.
Don't run two at once. The source files are kept in `-workdir` rather than
deleted, so a later encoder change can re-render the whole backfill from
source.

## Environment

See [.env.example](.env.example). `R2_PUBLIC_URL` is required — the server
refuses to boot without it. R2 bucket credentials live in PocketBase's S3
settings (admin UI), not in env.

## Build / deploy

```bash
go build -o myapp .
./myapp serve --http=0.0.0.0:8080
```

Production runs on a self-hosted Windows PC (Ryzen 7800X3D + RTX 4080 Super,
NVENC for the H.264 rendition) behind a Cloudflare Tunnel, under
[scripts/updater](scripts/updater/main.go): pushing to `main` is the deploy. The
updater builds the new commit, has the server drain in-flight uploads and bot
work before restarting (hooks/deploy.go), and rolls back if the new build
doesn't come up healthy. Deploys, rollbacks and failed builds are recorded in
`system_logs` (source `server`). Host setup:
[docs/self-host-windows.md](docs/self-host-windows.md).

It previously ran on a Fedora box (i5-10400F + Arc A310) with VAAPI; the same
code still picks VAAPI automatically where `/dev/dri` is reachable.

The [Dockerfile](Dockerfile) is stale: it targets alpine for Railway, has no
VAAPI runtime, and cannot reach a GPU. Either rebuild it on a Debian/Fedora
base with `intel-media-driver` and run it with `--device /dev/dri`, or drop it
in favour of a systemd unit.
