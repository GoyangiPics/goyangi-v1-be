# Forwarding Discord messages to goyangi

This is for the developer of a bot that already reads the channels goyangi
archives and wants to feed it, instead of goyangi running its own bot there.

**The short version:** forward each new message in the agreed channels,
exactly as Discord delivered it, together with the names of the roles it pings.
goyangi decides everything else and tells you what reaction to leave. Your bot
parses nothing, resolves nothing and downloads nothing.

That split is deliberate. goyangi's ingestion has a lot of rules: role pings,
replies, follow-up chains, idol detection from text, imgur albums, relay-app
credits, uploader matching, opt-outs. They already run for goyangi's own bot,
and the API runs the very same code, so a forwarded message is treated exactly
like one goyangi saw itself. If your bot reimplemented any of it, the two
would drift apart.

## Setup

| | |
|---|---|
| Base URL | `https://api.goyangi.pics` |
| Auth | `Authorization: Bearer <token>`. goyangi's admin gives you the token privately. Keep it out of logs and source control. |
| Intents your bot needs | Guilds, Guild Messages, **Message Content** (privileged). Without Message Content, Discord sends guild messages with empty `content` and `attachments`, and nothing can be ingested. |
| Which channels | goyangi's admin gives you the list. goyangi checks it again on its side, so forwarding an extra channel is harmless, just wasted requests. |

## What to forward

- **New messages only** (`MESSAGE_CREATE`). goyangi doesn't follow edits or
  deletes.
- **From everyone, other bots and webhooks included.** People post through relay
  apps, and goyangi credits the person behind the app when it can. Your bot's
  own messages can be forwarded too: goyangi ignores them when you send your
  bot's user id along with them.
- **Only messages that could carry media**, to save requests: ones with an
  attachment, or with `http` anywhere in `content`. Everything else is always
  ignored, so this filter loses nothing.
- **Promptly.** Discord's attachment URLs are signed and expire after about a
  day, and goyangi downloads from them.
- **Unmodified.** Don't strip mentions, `key: value` lines, embeds or anything
  else from the message. Those are the metadata.

## Request

```
POST /api/ingest/discord/messages
Authorization: Bearer <token>
Content-Type: application/json
```

```json
{
  "message": { "...": "the Discord message object, as delivered" },
  "roles": { "1253370470000000000": "Yujin [IVE]" },
  "bot_user_id": "1100000000000000000"
}
```

| Field | Required | What it is |
|---|---|---|
| `message` | yes | The message object exactly as the gateway delivered it: `MESSAGE_CREATE`'s `d`, with `guild_id`. Discord's own snake_case JSON, not your library's object model. |
| `roles` | when the message pings roles | Role id → role name, for **every** id in `message.mention_roles`. Discord only sends the ids, and the names are where idol and group come from (`Idol [Group]`). A pinged role without a name is refused with 400. Sending your whole guild role map is fine too. |
| `bot_user_id` | recommended | Your bot's user id. @-mentioning your bot becomes the explicit trigger (see below), and your bot's own messages are ignored. Without it there is no @-mention trigger. |

The request body is limited to 1 MB.

### Getting the raw message

**discord.js (v14)**, where `forward` POSTs the body and `react` is your own helper:

```js
client.on(Events.Raw, async (packet) => {
  if (packet.t !== 'MESSAGE_CREATE') return
  const m = packet.d
  if (!m.guild_id || !CHANNELS.has(m.channel_id)) return
  if (!m.attachments?.length && !m.content?.includes('http')) return

  const guild = client.guilds.cache.get(m.guild_id)
  const roles = Object.fromEntries(
    (m.mention_roles ?? []).map((id) => [id, guild?.roles.cache.get(id)?.name ?? '']),
  )
  const verdict = await forward({ message: m, roles, bot_user_id: client.user.id })
  if (verdict?.reaction === 'success') await react(m, '✅')
  if (verdict?.reaction === 'skipped') await react(m, '⏭️')
})
```

**discord.py (2.x):** raw payloads need `enable_debug_events=True` on the client.

```python
client = discord.Client(intents=intents, enable_debug_events=True)

@client.event
async def on_socket_raw_receive(raw: str):
    packet = json.loads(raw)
    if packet.get("t") != "MESSAGE_CREATE":
        return
    m = packet["d"]
    if not m.get("guild_id") or m["channel_id"] not in CHANNELS:
        return
    if not m.get("attachments") and "http" not in (m.get("content") or ""):
        return
    guild = client.get_guild(int(m["guild_id"]))
    roles = {rid: (guild.get_role(int(rid)).name if guild and guild.get_role(int(rid)) else "")
             for rid in m.get("mention_roles", [])}
    verdict = await forward({"message": m, "roles": roles, "bot_user_id": str(client.user.id)})
    ...
```

**Building the object yourself** works too, if your library won't give you the
raw payload. These are the fields goyangi reads:

- `id`, `channel_id`, `guild_id`
- `author`: `id`, `username`, `bot`, `system`
- `content`
- `attachments[]`: `id`, `filename`, `content_type`, `url`, `size`
- `mention_roles[]`
- `referenced_message`: at least its `id`, on replies
- `embeds[]`: relay apps put their "Posted by:" credit there
- `webhook_id`
- `interaction_metadata.user`: `id`, `username`, on messages a slash command produced
- `timestamp`

## Response

| HTTP | Meaning | What to do |
|---|---|---|
| 200 | The verdict, below. | React as `reaction` says. |
| 202 | `"status": "processing"`: still downloading after 25 seconds. | Poll `GET /api/ingest/discord/messages/{message id}` every few seconds. It answers 202 until the verdict is ready, then 200 with the verdict. Or just don't react. |
| 400 | Your request is malformed; `message` says how. | Fix the payload. Retrying the same thing won't help. |
| 401 | Missing or wrong token. | Check the token. |
| 404 on GET | No recent ingest for that message id. Verdicts are kept for an hour. | Nothing. |
| 5xx, timeout, connection error | Something on the way failed. | Retry with backoff. **Retries are safe**: the same message id gets the original verdict back, or `duplicate`, never a second set of posts. |

The 25-second wait exists because the API sits behind Cloudflare, which drops a
request after 100 seconds, and a message with several large videos can take
longer to download than that. The work carries on even if your connection
drops.

The verdict:

```json
{
  "message_id": "1426500000000000000",
  "status": "ingested",
  "reaction": "success",
  "created": 3,
  "total": 3,
  "set": "https://goyangi.pics/set/u2vdtzikm7zn61s"
}
```

| `status` | Meaning | `reaction` |
|---|---|---|
| `ingested` | At least one item became a post. `created` of `total`; `failed` lists items that didn't make it, `unresolved` lists names that matched nothing. | `success` |
| `skipped` | Deliberately not ingested, and `reason` says why: the poster turned off automatic import, the uploader is blocked, or it was a re-announcement (see below). | `skipped` |
| `ignored` | Not for goyangi: wrong channel, no trigger, no media, or your own message. | `""` |
| `duplicate` | Already handled: a retry, or goyangi's own bot got there first. | `""` |
| `failed` | Attempted, and nothing came of it. Details go to goyangi's logs. | `""` |

**Reactions:** `success` is ✅ (or your own emoji), `skipped` is ⏭️, and an
empty string means leave nothing. A failure is intentionally *not* marked with
❌: a red mark on someone's post reads as a public telling-off, and the problem
is goyangi's to fix, not the poster's. ⏭️ is shown because otherwise someone who
opted out, or was blocked, would keep re-posting without knowing why.

## What goyangi does with a message

You don't need to implement any of this. It's here so you know what a forwarded
message turns into, and why a field matters.

**Triggers.** Only in the agreed channels. One of these has to apply:

- **Role ping.** Idol and group come from the pinged role names, in the form
  `Idol [Group]`: `Yujin [IVE]`, or `Chae Won [LE SSERAFIM]` for multi-word
  names. If the stated group doesn't contain that idol but the name is unique
  in the directory, the idol wins. A group-only role like `@ifeye` falls through
  to text detection, with the role name added to the text.
- **@-mention of your bot** (needs `bot_user_id`). Metadata comes from
  `key: value` lines in the message:

  ```
  idol: Wonyoung, Yujin
  group: IVE
  tags: fancam, 4k
  title: Love Dive stage
  date: 240115          # or "today"; defaults to now
  source: https://youtu.be/...
  uploader: name        # defaults to the poster
  filetype: sticker     # image / gif / video / sticker
  mirror: https://...   # attribution only, never downloaded
  ```

  These lines also work alongside a role ping, and override what the roles say.
- **Reply** by the same author to a message that created a set: the new items
  join that set. This is why `referenced_message` matters.
- **Follow-up**: a message with media and no trigger of its own, from the same
  author in the same channel, shortly after their ping: within 5 minutes of the
  last message that joined, up to 3 follow-ups. It joins that set. A repeated
  ping about the same idols joins the same way.
- **Text detection**, the last resort: a message with media that names an idol
  goyangi knows, with no ping and no mention. Strict on purpose, and every hit
  is logged for review.

A message with **both** a role ping and an @-mention of your bot is **skipped**
(⏭️). That's how posters say "I already uploaded this on the site, the ping is
just so people see it".

**Media.** Discord attachments; imgur links and imgur albums and galleries
(every item in the album); catbox and pixeldrain links; and any direct link to
an mp4, webm, mov, webp, avif, jpg, png or gif. Links to goyangi's own content
are not re-downloaded.

**Who gets credited.**

1. An `uploader:` line, if the message has one.
2. Otherwise the author's `username`.
3. For a message from a relay app, the person who ran the app's command, when
   Discord says so (`interaction_metadata.user`). Failing that, a
   `Posted by: name` line in the message or its embeds, if the name matches an
   existing goyangi uploader.

Names match an uploader's name or any of its aliases. An unknown name gets a new
uploader on first sight.

**Opt-outs and blocks.** A goyangi uploader can turn off automatic import from
Discord, and an admin can block an uploader. Both come back as `skipped`.

**One message, one set.** Every item in a message lands in a single set
(stickers excepted), and the message's link is stored on each post. That's also
how duplicates are caught.

## Checklist

- [ ] Message Content intent on, both in the developer portal and in code.
- [ ] Forward `MESSAGE_CREATE` only, in the agreed channels, with attachments or `http` in content.
- [ ] Send the raw message object with `guild_id`; don't modify it.
- [ ] Send a name for every id in `mention_roles`.
- [ ] Send `bot_user_id`.
- [ ] React from `reaction`: `success` gets ✅, `skipped` gets ⏭️, anything else nothing.
- [ ] Retry on 5xx and timeouts with backoff; don't retry on 400.
- [ ] Optionally poll after a 202.
- [ ] Keep the token secret.

## Trying it

```bash
curl -s https://api.goyangi.pics/api/ingest/discord/messages \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"message":{"id":"1","channel_id":"2","guild_id":"3","author":{"id":"4","username":"test"},"content":"hello"},"roles":{}}'
```

That message is in no agreed channel and has no media, so it should come back
`"status": "ignored"`. For a real test, ask goyangi's admin to add a test
channel to the list.
