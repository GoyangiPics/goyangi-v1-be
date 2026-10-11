# Forwarding Discord messages to goyangi

For the developer of a bot that already reads the channels goyangi archives,
and forwards their messages to goyangi instead of goyangi running its own bot
in your server.

**The whole idea:** forward each new message in the agreed channels exactly as
Discord delivered it, with the names of the roles it pings. goyangi decides
everything else and tells you what reaction to leave. Your bot downloads
nothing and parses nothing; its only lookup is naming the pinged roles.

That split is deliberate. goyangi's archiving has a lot of rules — role pings,
replies, follow-ups, idol detection from text, imgur albums, crediting people
who post through other bots, opt-outs. They already run for goyangi's own bot,
and this API runs the very same code, so a forwarded message is treated exactly
like one goyangi saw itself. If your bot reimplemented any of it, the two would
drift apart.

Contract version 1 (October 2026). Contact: Goyangi, on Discord or at
goyangi.pics@protonmail.com.

**Contents**

1. [Quick start](#1-quick-start)
2. [Setup](#2-setup)
3. [What to forward](#3-what-to-forward)
4. [The request](#4-the-request)
5. [Sample code](#5-sample-code)
6. [Responses](#6-responses)
7. [Order, retries and limits](#7-order-retries-and-limits)
8. [Posts your bot makes for other people](#8-posts-your-bot-makes-for-other-people)
9. [Archiving a message on request](#9-archiving-a-message-on-request)
10. [Dry runs and testing](#10-dry-runs-and-testing)
11. [Going live](#11-going-live)
12. [What goyangi does with a message](#12-what-goyangi-does-with-a-message)
13. [Privacy](#13-privacy)
14. [Operations](#14-operations)
15. [Troubleshooting](#15-troubleshooting)
16. [Checklist](#16-checklist)

## 1. Quick start

1. Get the token and the list of channel ids from goyangi's admin, and give them
   your bot's user id.
2. On every `MESSAGE_CREATE` in those channels that has an attachment or a link,
   `POST https://api.goyangi.pics/api/ingest/discord/messages` with the raw
   message, the names of its pinged roles, and your bot's user id — **one
   message at a time per channel, in the order they arrive** (section 7).
3. React as the answer's `reaction` says: `success` → ✅, `skipped` → ⏭️,
   anything else → nothing.
4. Posts your bot makes for people (anonymous ones included) carry a `relay`
   block saying whose they are (section 8).
5. Optionally, offer "Archive on goyangi" on any message, like goyangi's own
   bot does (section 9).
6. Before going live, send everything with `"dry_run": true` for a while and
   compare (section 10).

## 2. Setup

| | |
|---|---|
| Base URL | `https://api.goyangi.pics` |
| Auth | `Authorization: Bearer <token>`. goyangi's admin gives you the token privately. Keep it out of logs and source control. |
| Your bot's id | goyangi's admin puts your bot's user id on file. Requests naming any other `bot_user_id` are refused. |
| Intents | Guilds, Guild Messages, **Message Content** (privileged — on in the developer portal *and* in code). Without it Discord sends guild messages with empty `content`, `embeds` and `attachments` — except messages that mention your bot, its own messages and DMs — so almost nothing arrives with anything to archive. |
| Permissions in the channels | View Channel, **Read Message History** and **Add Reactions**. Discord requires Read Message History to add any reaction, not just to catch up. |
| Channels | The exact channel ids from goyangi's admin. goyangi checks them again on its side, so an extra channel is harmless to goyangi — but it sends members' messages to a third party, so forward only the agreed ones. |

## 3. What to forward

- **New messages only** (`MESSAGE_CREATE`). goyangi doesn't follow edits or
  deletes (section 13 covers deleted messages).
- **From everyone, other bots and webhooks included.** People post through
  relay apps, and goyangi credits the person behind the app when it can. Your
  own bot's posts are the exception: send those as described in section 8, not
  from your general forwarder.
- **Only messages that could carry media**, to save requests: an attachment, or
  `http` anywhere in `content`. goyangi only recognises links written with
  `http://` or `https://`, so this filter loses nothing. (One cosmetic
  exception: a role ping plus an @-mention of your bot gets ⏭️ even without
  media; skipping it costs only that reaction.)
- **In order, one at a time per channel** — see section 7. This matters:
  follow-ups and replies only work when goyangi gets a channel's messages in
  the order they were posted.
- **Promptly.** Discord's attachment URLs expire after about a day, and goyangi
  downloads from them.
- **Unmodified.** Don't strip mentions, `key: value` lines, embeds or anything
  else. The only change you may make is adding `guild_id` to a message you
  fetched over REST.
- **Threads and forum posts** have their own channel id, which isn't the
  parent's. They're only archived if goyangi's admin lists that thread's id;
  otherwise they come back `ignored` / `channel_not_listed`.

goyangi does **not** take: media only visible in an embed (a link preview),
Discord's own stickers, polls, or messages forwarded with Discord's Forward
button (their content is in `message_snapshots`, which isn't read). Voice
messages and other non-media files are attempted and fail.

## 4. The request

```
POST /api/ingest/discord/messages
Authorization: Bearer <token>
Content-Type: application/json
```

```json
{
  "message": {
    "id": "1426543210987654321",
    "channel_id": "1170388502379249747",
    "guild_id": "1100000000000000000",
    "author": { "id": "300000000000000000", "username": "nabi", "bot": false },
    "content": "<@&1253370470000000000> stage today",
    "mention_roles": ["1253370470000000000"],
    "attachments": [
      {
        "id": "1426543210987654322",
        "filename": "yujin.mp4",
        "content_type": "video/mp4",
        "size": 5242880,
        "url": "https://cdn.discordapp.com/attachments/1170388502379249747/1426543210987654322/yujin.mp4?ex=…&is=…&hm=…"
      }
    ],
    "embeds": [],
    "timestamp": "2026-10-11T12:00:00.000000+00:00",
    "type": 0
  },
  "roles": { "1253370470000000000": "Yujin [IVE]" },
  "bot_user_id": "1200000000000000000"
}
```

| Field | Required | What it is |
|---|---|---|
| `message` | yes | The message object exactly as the gateway delivered it: `MESSAGE_CREATE`'s `d`, with `guild_id`. Discord's own snake_case JSON — not your library's object model. Ids are strings, as Discord sends them; a numeric id is a 400. |
| `roles` | when the message pings roles | Role id → role name, for every id in `message.mention_roles`. Discord only sends the ids, and the names are where idol and group come from (`Idol [Group]`). A pinged role you send no name for is left out, and the answer carries a warning. Sending your whole guild role map is fine. |
| `bot_user_id` | yes | Your bot's user id, as a string — the one goyangi's admin has on file. goyangi treats it as one of its own bots: its posts are never archived as they are (section 8), and `<@your bot>` is a mention of goyangi (section 12). |
| `relay` | no | Only on a post your bot made for someone. See section 8. |
| `dry_run` | no | `true`: decide everything, save nothing. See section 10. |

goyangi only reads these fields of the message:

- `id` (also the posting time), `channel_id`, `guild_id`, `content`
- `author`: `id`, `username`, `bot`, `system`
- `attachments[]`: `filename`, `content_type`, `url`
- `mention_roles[]`
- `referenced_message.id`, or `message_reference.message_id` and `.type`, on replies
- `embeds[]`: `description`, `fields`, `footer` (relay apps put a "Posted by:" credit there)
- `webhook_id`, `application_id`
- `interaction_metadata.user` and the older `interaction.user`: `id`, `username`

`type` and `timestamp` aren't read. Send the whole object anyway — it's simplest
— but it's decoded in full, so a malformed value in any field is a 400. It must
stay within Discord's own limits: at most 10 attachments, 4000 characters of
`content` (counted as Unicode code points, so Korean text is fine), and 10
embeds; every attachment URL must be `https://` on `cdn.discordapp.com` or
`media.discordapp.net`. The body is limited to 1 MiB.

## 5. Sample code

Sketches of the whole client: forwarding in order, role names, retries,
collecting a verdict and reacting. Read and adapt them; don't paste them blind.

**discord.js (v14, Node 18+):**

```js
import { Client, Events, GatewayIntentBits } from 'discord.js'

const API = 'https://api.goyangi.pics/api/ingest/discord/messages'
const TOKEN = process.env.GOYANGI_INGEST_TOKEN
const CHANNELS = new Set(['1170388502379249747']) // ids as strings, as in the payload
const OWN_WEBHOOKS = new Set([]) // ids of webhooks your bot posts through, if any
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

const client = new Client({
  intents: [GatewayIntentBits.Guilds, GatewayIntentBits.GuildMessages, GatewayIntentBits.MessageContent],
})

// One lane per channel: a channel's next message is sent only once goyangi
// has answered the previous one (retries included), so they arrive in order.
const lanes = new Map()

client.on(Events.Raw, (packet) => {
  if (packet.t !== 'MESSAGE_CREATE') return
  const m = packet.d
  if (!m.guild_id || !CHANNELS.has(m.channel_id)) return
  if (m.author?.id === client.user.id || OWN_WEBHOOKS.has(m.webhook_id)) return // your own posts: section 8
  if (!m.attachments?.length && !m.content?.includes('http')) return

  const posted = (lanes.get(m.channel_id) ?? Promise.resolve()).then(() => post(m))
  lanes.set(m.channel_id, posted.catch(() => {}))
  // Waiting for a verdict happens outside the lane. Never let a failure become
  // an unhandled rejection: it would take the whole bot down.
  posted.then((res) => settle(m, res)).catch((err) => console.error('goyangi: forward failed', m.id, err))
})

async function roleNames(guildId, ids = []) {
  const guild = client.guilds.cache.get(guildId)
  const names = {}
  for (const id of ids) {
    const role = await guild?.roles.fetch(id).catch(() => null) // the cache first, then Discord
    if (role) names[id] = role.name
  }
  return names
}

async function post(m) {
  const body = JSON.stringify({
    message: m,
    roles: await roleNames(m.guild_id, m.mention_roles),
    bot_user_id: client.user.id,
  })
  return call(API, { method: 'POST', body })
}

// One call, retried on timeouts, network errors, 429 and 5xx for up to 30
// minutes — all safe to repeat (section 7). Anything else is final.
async function call(url, init = {}) {
  const giveUp = Date.now() + 30 * 60_000
  for (let delay = 5_000; ; delay = Math.min(delay * 2, 300_000)) {
    let wait = delay
    try {
      const res = await fetch(url, {
        ...init,
        headers: { Authorization: `Bearer ${TOKEN}`, 'Content-Type': 'application/json', ...init.headers },
        signal: AbortSignal.timeout(40_000),
      })
      if (res.status < 500 && res.status !== 429) return res
      wait = Number(res.headers.get('retry-after')) * 1000 || delay
      await res.body?.cancel()
    } catch (err) {
      if (Date.now() + wait > giveUp) throw err
    }
    if (Date.now() + wait > giveUp) throw new Error(`goyangi: gave up on ${url}`)
    await sleep(wait)
  }
}

// A 202: check back every 10 s for up to an hour. A 404 there means goyangi
// restarted; sending the same request again is safe.
async function settle(m, res) {
  for (let i = 0; res.status === 202 && i < 360; i++) {
    await res.body?.cancel()
    await sleep(10_000)
    res = await call(`${API}/${m.id}`)
    if (res.status === 404) {
      await res.body?.cancel()
      res = await post(m)
    }
  }
  if (res.status !== 200) {
    console.warn('goyangi: no verdict', m.id, res.status, await res.text())
    return
  }
  const verdict = await res.json()
  const emoji = { success: '✅', skipped: '⏭️' }[verdict.reaction]
  if (emoji) await client.channels.cache.get(m.channel_id)?.messages.react(m.id, emoji)
}

client.login(process.env.DISCORD_TOKEN)
```

**discord.py (2.x):** raw payloads need `enable_debug_events=True`, and
`on_socket_raw_receive` receives every gateway frame as a `str`.

```python
import asyncio, json, logging, os, time
from collections import defaultdict
import aiohttp, discord

API = "https://api.goyangi.pics/api/ingest/discord/messages"
TOKEN = os.environ["GOYANGI_INGEST_TOKEN"]
CHANNELS = {"1170388502379249747"}  # ids as strings, as in the payload
OWN_WEBHOOKS = set()  # ids of webhooks your bot posts through, if any

intents = discord.Intents.default()
intents.message_content = True
client = discord.Client(intents=intents, enable_debug_events=True)

# One lane per channel: a channel's next message is sent only once goyangi has
# answered the previous one (retries included). asyncio.Lock wakes waiters in
# order, and nothing awaits before the lock, so lanes follow the gateway's order.
lanes = defaultdict(asyncio.Lock)
_session = None


def session():
    global _session  # one session for everything: a new one per request is slow
    if _session is None:
        _session = aiohttp.ClientSession(
            headers={"Authorization": f"Bearer {TOKEN}"}, timeout=aiohttp.ClientTimeout(total=40)
        )
    return _session


@client.event
async def on_socket_raw_receive(raw: str):
    if '"MESSAGE_CREATE"' not in raw:  # cheap: this fires for every frame
        return
    packet = json.loads(raw)
    if packet.get("t") != "MESSAGE_CREATE":
        return
    m = packet["d"]
    if not m.get("guild_id") or m["channel_id"] not in CHANNELS:
        return
    if m["author"]["id"] == str(client.user.id) or m.get("webhook_id") in OWN_WEBHOOKS:
        return  # your own posts: section 8
    if not m.get("attachments") and "http" not in (m.get("content") or ""):
        return
    try:
        async with lanes[m["channel_id"]]:
            status, verdict = await post(m)
        await settle(m, status, verdict)  # outside the lane
    except Exception:
        logging.exception("goyangi: forward failed for %s", m["id"])


async def role_names(guild_id, ids):
    guild = client.get_guild(int(guild_id))
    names = {rid: role.name for rid in ids if guild and (role := guild.get_role(int(rid)))}
    missing = [rid for rid in ids if rid not in names]
    if missing and guild:
        try:
            fetched = {str(r.id): r.name for r in await guild.fetch_roles()}
            names.update({rid: fetched[rid] for rid in missing if rid in fetched})
        except discord.HTTPException:
            pass  # goyangi says which roles had no name, in `warnings`
    return names


async def post(m):
    body = {
        "message": m,
        "roles": await role_names(m["guild_id"], m.get("mention_roles", [])),
        "bot_user_id": str(client.user.id),
    }
    return await call("POST", API, json=body)


# One call, retried on timeouts, network errors, 429 and 5xx for up to 30
# minutes — all safe to repeat (section 7). Anything else is final.
async def call(method, url, **kw):
    give_up, delay = time.monotonic() + 30 * 60, 5
    while True:
        wait = delay
        try:
            async with session().request(method, url, **kw) as res:
                if res.status < 500 and res.status != 429:
                    try:
                        return res.status, await res.json(content_type=None)
                    except ValueError:  # not JSON, e.g. an error page on the way
                        return res.status, None
                retry_after = res.headers.get("Retry-After", "")
                wait = int(retry_after) if retry_after.isdigit() else delay
        except (aiohttp.ClientError, asyncio.TimeoutError):
            pass
        if time.monotonic() + wait > give_up:
            raise RuntimeError(f"goyangi: gave up on {url}")
        await asyncio.sleep(wait)
        delay = min(delay * 2, 300)


async def settle(m, status, verdict):
    # A 202: check back every 10 s for up to an hour. A 404 there means goyangi
    # restarted; sending the same request again is safe.
    for _ in range(360):
        if status != 202:
            break
        await asyncio.sleep(10)
        status, verdict = await call("GET", f"{API}/{m['id']}")
        if status == 404:
            status, verdict = await post(m)
    if status != 200 or not verdict:
        logging.warning("goyangi: no verdict for %s (HTTP %s): %s", m["id"], status, verdict)
        return
    emoji = {"success": "✅", "skipped": "⏭️"}.get(verdict.get("reaction"))
    if emoji:
        channel = client.get_partial_messageable(int(m["channel_id"]))
        await channel.get_partial_message(int(m["id"])).add_reaction(emoji)


client.run(os.environ["DISCORD_TOKEN"])
```

**Another library:** any library can give you the raw gateway payload. If
yours really can't, build the object from the field list in section 4.

## 6. Responses

### HTTP status

| HTTP | Meaning | What to do |
|---|---|---|
| 200 | The verdict, below. | React as `reaction` says. |
| 202 | `"status": "processing"`: accepted, not finished. goyangi answers 202 after 25 seconds if the message is still downloading — or after ~3 seconds if it's waiting behind earlier messages in its channel. | Move on to the channel's next message. Collect the verdict with `GET /api/ingest/discord/messages/{message id}` (same token): 202 while it's running, then 200 with the verdict. |
| 400 | The request is wrong; the error's `message` says how. Nothing was done. | Fix the payload. Retrying the same request won't help. |
| 401 | Missing or wrong token. | Check the token (section 14). |
| 404 on POST | The API is switched off, or the path is wrong. | Don't retry; tell goyangi's admin. |
| 404 on GET | goyangi holds no verdict for that id: an hour since it finished, goyangi restarted, or it was never accepted (a 400 or 503, or a dry run, which isn't kept). | If you were waiting on a 202, send the same POST again — it's safe. |
| 413 | The body is over 1 MiB. A real message object is a few KB, so the wrong thing is being sent. | Don't retry. |
| 429 | Rate-limited. Only possible if goyangi turns rate limiting on. | Wait `Retry-After` if given, otherwise 10–30 seconds; retry. |
| 503 | Nothing was done: goyangi is restarting (`Retry-After: 60` — a restart waits for running work, so this can last several minutes), or 200 messages are already waiting (`Retry-After: 30`). | Retry after `Retry-After`. |
| other 5xx, timeout, connection error | Something on the way failed. Cloudflare's own errors may come back as HTML, not JSON. | Retry with backoff (section 7). |

Errors have PocketBase's shape:

```json
{ "status": 400, "message": "`bot_user_id` is required: your bot's user id.", "data": {} }
```

### The verdict

```json
{
  "message_id": "1426543210987654321",
  "status": "ingested",
  "reaction": "success",
  "created": 2,
  "total": 3,
  "failed": ["huge-fancam.mp4"],
  "set": "https://goyangi.pics/set/261011-ive-yujin-1a2b3c4d"
}
```

| Field | When | What |
|---|---|---|
| `message_id` | always | The message's id. |
| `status` | always | See the next table. |
| `reason` | when there is one | A stable code, see the next table. On a POST's 202 it's `still_running`; a GET's 202 has none. |
| `reaction` | always | `"success"`, `"skipped"` or `""` (leave nothing). |
| `created`, `total` | always | Posts made, out of the items goyangi found in the message. Both 0 when it didn't get as far as looking (`ignored`, `duplicate`, a re-announcement); in a dry run `created` is 0 and `total` is what would be fetched. |
| `failed` | when some items failed | Their filenames — for a link, a name taken from it. Why is in goyangi's logs. |
| `unresolved` | when names matched nothing | e.g. `"idol Wonyoung"`, `"group NewJeans"`. Posts were still made if at least one idol and one group matched. |
| `set` | when posts were made in a set | The set they're in. Absent for `filetype: sticker` posts, which get no set. Items may show as processing for a few minutes while they encode. |
| `warnings` | when the request had problems that didn't stop it | e.g. a pinged role sent without a name. Worth logging: it usually means your role cache missed. |
| `reply` | manual requests | Section 9. |
| `dry_run`, `preview` | dry runs | Section 10. |

Unknown fields may appear later; ignore them.

| `status` | `reason` | Meaning | `reaction` |
|---|---|---|---|
| `ingested` | — | At least one item became a post. Partial runs count: check `failed`. | `success` |
| `skipped` | `reannouncement` | A role ping plus an @-mention of the bot: "already on the site". | `skipped` |
| | `opted_out` | The credited uploader turned off automatic import from Discord. | `skipped` |
| | `blocked` | goyangi's admin blocked the credited uploader. | `skipped` |
| `ignored` | `channel_not_listed` | Not an archived channel (threads have their own ids). | `""` |
| | `no_media` | Nothing goyangi can archive in it. | `""` |
| | `no_trigger` | Media, but nothing to go on: no usable role ping or `idol:`/`group:` lines, not a reply or follow-up, and no idol named in the text. | `""` |
| | `own_message` | A post by your bot (without `relay`) or by goyangi's bot. | `""` |
| | `system_message` | A message whose author is Discord's system user. | `""` |
| `duplicate` | `already_handled` | Handled within the last hour — by goyangi's own bot, or an earlier request. | `""` |
| | `already_archived` | Posts from this message are already on the site. | `""` |
| `failed` | `unresolved_names` | No idol or no group matched anything goyangi knows. No posts were made. | `""` |
| | `nothing_saved` | Every item failed (download error, file too big, unsupported type). | `""` |
| | `internal_error` | goyangi's problem; it's logged. | `""` |
| `processing` | `still_running` | Only on a POST's 202. | `""` |
| `denied` | … | Only manual requests (section 9). | `""` |

**Reactions:** `success` is ✅ (or an emoji of your own), `skipped` is ⏭️, and
an empty string means leave nothing. A failure is deliberately *not* marked
with ❌: a red mark on someone's post reads as a public telling-off, and the
problem is goyangi's to look into, not the poster's. ⏭️ is shown because
otherwise someone who opted out, or was blocked, keeps re-posting without
knowing why.

New `status` and `reason` values may be added later. Treat any you don't
recognise as "no reaction".

## 7. Order, retries and limits

**Send each channel's messages in order, one at a time.** goyangi works through
each channel's messages one at a time, in the order the *requests reach it*,
and different channels don't wait for each other. Follow-ups, replies and
repeat pings need the message before them handled first; one that arrives
before its opener comes back `ignored` / `no_trigger` and is lost. Requests sent
in parallel can arrive swapped — a retry, a role lookup or plain network timing
is enough. So keep a lane per channel, as both samples do: send a channel's next
message only after the previous POST has answered (200, 202 or a 4xx), retries
included. A 202 means the message has its place in line, so move on and collect
the verdict in the background.

**Timeouts.** goyangi holds a POST for up to 25 seconds, so set your HTTP
timeout to 30–40 seconds.

**Resending is always safe.** goyangi never makes a second copy of a post:

- The same message id gets the same verdict for an hour after it finished —
  including while it's still running (a 202 again).
- `ignored` and `failed` are the exceptions: a resend is looked at afresh, so
  a download that failed during an outage can succeed later.
- After an hour, or after goyangi restarts, a message that has posts on the
  site answers `duplicate` / `already_archived`.
- If only some of a message's items were saved (others failed, or goyangi
  stopped mid-download), a resend doesn't fetch the rest.

**Recommended policy:**

| Got | Do |
|---|---|
| timeout, connection error, 5xx, 503 | Retry with backoff (5 s, doubling, at most 5 minutes apart; honour `Retry-After`) for **at least 30 minutes**, holding the channel's later messages meanwhile. Then give up, log it, and catch the message up later. |
| 429 | Wait `Retry-After`, or 10–30 seconds; retry. |
| 202 | Move on; poll the GET every ~10 seconds for up to an hour (time spent behind earlier messages counts). |
| `failed` / `nothing_saved` | Optionally resend once after a minute or two — not when the items were voice messages or other non-media files. |
| 400, 401, 404 on POST, 413 | Don't retry. Log it and tell goyangi's admin. |

**Queue.** At most 200 messages may be waiting or running at once, across all
channels; past that, a POST answers 503 with `Retry-After: 30` until the queue
drains.

**How long a message takes.** Each file gets up to 3 minutes and 100 MB; files
are fetched one after another, and an imgur album adds up to ~30 seconds to
look up. A typical message is done in seconds; ten large videos can take half
an hour, which is what the 202 is for.

**Catching up after downtime.** goyangi never fetches missed messages itself.
To catch up a channel:

1. Fetch `GET /channels/{id}/messages?after={last forwarded id}&limit=100`.
   Sort each page oldest first (by id), send it through the channel's lane, and
   ask again with `after` = the newest id you've sent, until a page comes back
   empty. Add `guild_id` to each — REST leaves it out.
2. Store the last forwarded id per channel on disk, so you know where to start.
3. Hold the channel's live messages until its backlog has gone, so the order
   holds.

Fetched messages carry freshly signed attachment URLs, and follow-ups are
matched on when messages were *posted*, so a backlog groups correctly. But
posts are dated when goyangi archives them, so catch up automatically only for
gaps of up to **6 hours**. For longer ones, ask goyangi's admin: there's a
separate backfill that keeps the real dates.

## 8. Posts your bot makes for other people

When your bot posts content for someone — "post this for me", or anonymously —
the message is authored by your bot (or its webhook), and the message itself
doesn't say who it was for. Your bot knows, because it made the post, so it
says so in a `relay` block:

```json
"relay": { "user": { "id": "300000000000000000", "username": "hyein" } }
```

That post is archived exactly as if the person had posted it themselves:
credited to their Discord **username** (not display name), their follow-ups and
replies through your bot join their sets, and their opt-out applies.
`relay.user.id` is how their messages are matched; goyangi keeps it in memory
for up to an hour and never stores it.

**Anonymous posts are credited too.** Anonymous means anonymous in your
server: on goyangi, an anonymous post is credited to the person who made it,
like any other. Send `relay.user` for anonymous posts as well. That's the
agreed behaviour, so say it where people choose to post anonymously — for
example "Posts without your name here. goyangi.pics still credits you."

```json
"relay": { "anonymous": true }
```

is for the rare post that should be credited to nobody: it's archived under
your bot's (or webhook's) username, and never joins another post's set.

How to send it:

- **Forward each such post once, with `relay`.** The simplest way: forward it
  from the code that posts it, right after posting, using the message object
  Discord returns — add `guild_id`, and for a webhook execute it with
  `?wait=true` to get the message back. Keep your general forwarder from also
  sending it (both samples skip your bot's own messages, and messages from the
  webhooks it posts through — other people's webhooks are forwarded as usual):
  a copy without `relay` would be ignored (your bot) or credited to the
  webhook's name (a webhook), and the correct one would then be a duplicate.
- **Order still matters:** send it through the channel's lane like any other
  message.
- **Take anonymous submissions privately** — by DM, slash command or modal. If
  people post in an archived channel and your bot deletes and reposts the
  message, the original may already have been archived by then, and the
  repost becomes a second copy.
- `relay` is accepted on messages your bot posted: as itself
  (`author.id` = `bot_user_id`), as an interaction response or follow-up, or
  through a webhook. On anything else it's a 400.
- With `relay`, goyangi ignores the message's other credit signals
  (`interaction_metadata`, "Posted by:" lines). An `uploader:` line the person
  wrote still wins.

## 9. Archiving a message on request

goyangi's own bot has **Ingest this message** — a right-click command on any
message — and `/reupload`: a trusted member picks a message, in any channel,
names the idols, and goyangi archives it credited to them. Your bot can offer
the same thing. goyangi does all the checking; your bot asks the question and
shows the answer.

### The request

```
POST /api/ingest/discord/manual
Authorization: Bearer <token>
Content-Type: application/json
```

```json
{
  "message": { "…": "the message to archive, as Discord delivered it, plus guild_id" },
  "bot_user_id": "1200000000000000000",
  "requested_by": {
    "id": "300000000000000000",
    "username": "nabi",
    "global_name": "Nabi 🐱",
    "roles": ["1300000000000000000"]
  },
  "idols": ["Yujin", "Gaeul — IVE"],
  "tags": ["fancam"],
  "set": "https://goyangi.pics/set/261011-ive-yujin-1a2b3c4d"
}
```

| Field | Required | What it is |
|---|---|---|
| `message` | yes | The message to archive, under the same rules as section 4. A message fetched over REST lacks `guild_id`: add it. |
| `bot_user_id` | yes | Your bot's user id. |
| `requested_by` | yes | The person who ran your command: `id`, `username`, `global_name` (their display name, if they have one) and `roles` (their role ids in this server). |
| `idols` | yes | Idol names, as typed or picked. A name shared by several groups takes its group after an em dash: `"Gaeul — IVE"`. goyangi record ids work too. |
| `tags` | no | Tags. They replace any `tags:` line in the message. |
| `set` | no | A goyangi set link: add the items to that set instead of a new one. |
| `dry_run` | no | Run every check, save nothing, and answer with a preview (section 10). |

### Who may ask

goyangi checks, in this order:

1. **Role.** If goyangi's admin has set roles allowed to archive on request,
   `requested_by.roles` must include one. Otherwise: `denied` /
   `no_permission`. Ask goyangi's admin which roles those are, and limit your
   command to them, so nobody sees an option that will refuse them.
2. **Account.** The person's display name or username must match a goyangi
   uploader that's linked to a goyangi account allowed to upload. Otherwise:
   `denied` / `cannot_upload`, and `reply` says which link is missing.

### What happens

- Any channel in your server: the agreed-channels list doesn't apply, because
  someone picked this message on purpose.
- Credited to the person who asked, not the message's author.
- Dated with the message's own date (a `date:` line in the message wins).
- The idols sent replace the message's `idol:`/`group:` lines, and the groups
  come from the idols. `tags` replace a `tags:` line. The message's other
  `key: value` lines (`title:`, `source:`, `filetype:`, `mirror:`) still apply.
- A new set, or the one `set` links to.
- A message already on the site isn't archived again: `duplicate` /
  `already_archived`, with a reply saying to edit the post on the site.
- A blocked uploader gets `skipped` / `blocked`. Opt-outs from automatic
  import don't apply: this is someone asking on purpose.

### The answer

The same shape as section 6, plus `reply`: the text goyangi's own bot would show
the person, ready to show them — privately, as an ephemeral reply. React on the
archived message as `reaction` says, the same as for forwarded messages.

| `status` | `reason` | `reply`, for example |
|---|---|---|
| `ingested` | — | ✅ Ingested 2 item(s). — or — ⚠️ Ingested 2/3 items. Failed: clip.mp4 |
| `denied` | `no_permission` | You don't have permission to use this command. |
| | `cannot_upload` | Your Discord name isn't linked to a goyangi uploader. Ask an admin to add it. |
| `failed` | `unknown_idols` | ❌ I don't know: Yujn. (the names are in `unresolved` too) |
| | `no_idols` | ❌ Pick at least one idol. |
| | `bad_set` | ❌ I couldn't find that set — double-check the link. |
| | `no_media` | ❌ No ingestible media found in that message (attachments or supported links). |
| | `own_message` | ❌ The bot's own messages can't be ingested. (goyangi's bot) |
| | `nothing_saved` | ❌ No items could be ingested: clip.mp4 |
| | `internal_error` | ❌ Something went wrong on goyangi's side. Try again shortly. |
| `skipped` | `blocked` | ⏭️ Not ingested — nabi is blocked from ingestion. |
| `duplicate` | `already_handled` | That message was already ingested recently. |
| | `already_archived` | That message is already on goyangi — edit the post on the site instead. |

A 202 and `GET /api/ingest/discord/manual/{message id}` work as in section 6
(manual requests wait the full 25 seconds; they aren't queued behind a
channel). Resending is safe: the same request gets the same answer for an hour,
except `denied` and `failed`, which are checked again — so someone whose account
was just linked can simply try again.

### Building the command

- **A message context-menu command** ("Archive on goyangi") is the closest
  match to goyangi's own. Discord delivers the target message with the command —
  content and attachments included, even to a bot without the Message Content
  intent.
- **Ask with a modal**: Idols (required), Tags and Set link (optional). Show it
  in response to the command; the submit arrives as a new interaction that
  doesn't carry the message. Keep what was right-clicked in memory for a few
  minutes, under a random key you put in the modal's `custom_id`. Don't put the
  message id itself there: a client can edit a `custom_id`.
- **Defer the submit as ephemeral**, send the request, then edit the reply with
  `reply`. On a 202, show something like "Still downloading — this will
  update." and poll.
- **A slash command with a message link**, like `/reupload`, works too: fetch the
  message over REST (which needs the Message Content intent), check it's from
  the same server, add `guild_id`. Slash-command options can autocomplete
  idols; modals can't.
- **Idol suggestions:** goyangi's idol list is public. For example
  `GET https://api.goyangi.pics/api/collections/groups_idols/records?filter=(name~'yu'||aliases~'yu')&expand=group&fields=id,name,expand.group.name&perPage=25`.
  Show `Name — Group`, and send what the person picked.
- **Try it with `dry_run` first:** every check runs, and the preview shows what
  would be archived and to whom it would be credited.

A sketch in discord.js v14, reusing `call` and `sleep` from section 5:

```js
import { randomUUID } from 'node:crypto'
import { ActionRowBuilder, Events, MessageFlags, ModalBuilder, Routes, TextInputBuilder, TextInputStyle } from 'discord.js'

// Registered once, as a message command:
//   new ContextMenuCommandBuilder().setName('Archive on goyangi').setType(ApplicationCommandType.Message)

const MANUAL = 'https://api.goyangi.pics/api/ingest/discord/manual'
const asked = new Map() // modal key → the message that was right-clicked

client.on(Events.InteractionCreate, async (i) => {
  try {
    if (i.isMessageContextMenuCommand() && i.commandName === 'Archive on goyangi') {
      const key = randomUUID()
      asked.set(key, { guildId: i.guildId, channelId: i.channelId, messageId: i.targetId })
      setTimeout(() => asked.delete(key), 15 * 60_000)
      const field = (id, label, required) =>
        new ActionRowBuilder().addComponents(
          new TextInputBuilder().setCustomId(id).setLabel(label).setStyle(TextInputStyle.Short).setRequired(required),
        )
      await i.showModal(
        new ModalBuilder()
          .setCustomId(`goyangi:${key}`)
          .setTitle('Archive on goyangi')
          .addComponents(
            field('idols', 'Idols (comma-separated)', true),
            field('tags', 'Tags (optional)', false),
            field('set', 'Add to a goyangi set (optional link)', false),
          ),
      )
      return
    }

    if (i.isModalSubmit() && i.customId.startsWith('goyangi:')) {
      const target = asked.get(i.customId.slice('goyangi:'.length))
      if (!target) return i.reply({ content: 'That took too long — run the command again.', flags: MessageFlags.Ephemeral })
      await i.deferReply({ flags: MessageFlags.Ephemeral })

      // The raw API object, with freshly signed attachment URLs.
      const message = await client.rest.get(Routes.channelMessage(target.channelId, target.messageId))
      const list = (s) => s.split(',').map((x) => x.trim()).filter(Boolean)
      const roles = Array.isArray(i.member.roles) ? i.member.roles : [...i.member.roles.cache.keys()]
      const body = JSON.stringify({
        message: { ...message, guild_id: target.guildId },
        bot_user_id: client.user.id,
        requested_by: { id: i.user.id, username: i.user.username, global_name: i.user.globalName, roles },
        idols: list(i.fields.getTextInputValue('idols')),
        tags: list(i.fields.getTextInputValue('tags')),
        set: i.fields.getTextInputValue('set').trim(),
      })

      let res = await call(MANUAL, { method: 'POST', body })
      if (res.status === 202) await i.editReply('Still downloading — this will update.')
      for (let n = 0; res.status === 202 && n < 360; n++) {
        await res.body?.cancel()
        await sleep(10_000)
        res = await call(`${MANUAL}/${target.messageId}`)
      }
      const answer = res.status === 200 ? await res.json() : null
      await i.editReply(answer?.reply ?? "goyangi didn't answer. Try again in a minute.")
      const emoji = { success: '✅', skipped: '⏭️' }[answer?.reaction]
      if (emoji) await client.channels.cache.get(target.channelId)?.messages.react(target.messageId, emoji)
    }
  } catch (err) {
    console.error('goyangi: archive command failed', err)
  }
})
```

The sketch fetches the message again after the modal (your bot has the intent,
and it gets fresh attachment URLs). A bot without the intent would keep the
message Discord delivered with the command instead — from the raw
`INTERACTION_CREATE` payload's `data.resolved.messages`.

## 10. Dry runs and testing

Add `"dry_run": true` to any request — forwarded or manual — and goyangi makes
every decision it would make, and changes nothing: no posts, no downloads, no
records, the message isn't claimed, and your bot isn't registered. A dry run
isn't queued or kept: it answers in the same request (never 202), so a GET for
its id is a 404, and the same id can be dry-run as often as you like. Its
`reaction` is always `""`.

```json
{
  "message_id": "1426543210987654321",
  "status": "ingested",
  "reaction": "",
  "created": 0,
  "total": 1,
  "dry_run": true,
  "preview": {
    "trigger": "role_ping",
    "uploader": "nabi",
    "title": "Yujin - IVE",
    "idols": ["Yujin"],
    "groups": ["IVE"],
    "items": [{ "url": "https://cdn.discordapp.com/attachments/…/yujin.mp4?…", "filename": "yujin.mp4", "filetype": "gif" }]
  }
}
```

- `status` and `reason` say what *would* happen. The `preview` is there for any
  message goyangi got as far as planning — including duplicates and opt-out or
  block skips — but not for one that's `ignored`, a re-announcement, or refused
  before that.
- `preview.trigger` is `role_ping`, `repeat_ping`, `mention`, `reply`,
  `follow_up`, `text`, or `manual`. `preview.joins_set` names the set a reply,
  follow-up or repeat ping would join.
- imgur albums aren't looked up in a dry run: they appear in `items` as
  themselves, with `filetype` `"album"`.
- It can't predict a download failing.
- `duplicate` doesn't mean disagreement: during a shadow run, goyangi's own bot
  usually took the message first. Compare the preview with what that bot did.
- A dry run never opens a set, so a follow-up or reply only shows as joining
  if goyangi's own bot (or a real request) had *finished* the message before it.
  Otherwise a dry-run follow-up comes back `ignored` / `no_trigger` (or `text`,
  if it names an idol), and a repeat ping shows as `role_ping`.

**Testing without touching the archive:** run your bot in dry-run mode on the
real channels for a few days next to goyangi's own bot, and compare its
previews with what goyangi's bot actually did (its reactions on the same
messages). A test channel with real requests creates real posts on the site;
only do that with goyangi's admin, who'll list the channel and clean up.

## 11. Going live

Agreed with goyangi's admin:

1. **Setup.** goyangi's admin turns the API on, puts your bot's user id on file,
   and gives you the token.
2. **Shadow.** Your bot forwards everyday messages with `"dry_run": true` while
   goyangi's own bot keeps archiving. From step 1 on, goyangi's bot leaves your
   bot's own posts to you, so send posts your bot makes for people (section 8)
   **for real** — nobody else will archive them.
3. **Switch.** Same moment: you drop `dry_run`, and goyangi's admin removes
   goyangi's bot from the server. If the two overlap for a while, nothing is
   archived twice — a message both bots see is taken once — but a clean switch
   is easier to reason about.
4. **Tell members** that the bot to @-mention is now yours: "@yourbot +
   `idol:`/`group:` lines" to ask for archiving, "role ping + @yourbot" for
   "already on the site, don't archive".

## 12. What goyangi does with a message

You don't need to implement any of this. It's here so you know what a
forwarded message turns into, and why the fields matter.

### Triggers

Only in the agreed channels, and checked in this order:

1. **Role ping + @-mention of the bot** → `skipped` / `reannouncement`, media
   or not. The "already on the site, the ping is just so people see it" signal.
2. **@-mention of the bot**: a literal `<@id>` of your bot (or goyangi's) in
   `content` — any mention counts, whatever it was meant for; a reply-ping
   doesn't. Metadata comes from `key: value` lines, which must give both
   `idol:` and `group:`. Without them, the message carries on through the rules
   below as if it had no mention.
3. **Role ping.** Idol and group come from the pinged role names, in the form
   `Idol [Group]`: a space before the bracket, nothing after the `]`
   (`Yujin [IVE]`, `Chae Won [LE SSERAFIM]`). Roles not in that form are
   ignored. If the stated group doesn't contain that idol but the name (or an
   alias) matches exactly one idol, her real group is added, alongside the
   stated group if goyangi knows it too. If no role yields both an idol and a
   group, the message goes straight to text detection (rule 7), with the role
   names added to the text — so `@ifeye` plus "Rahee" in the text works.
4. **Repeat ping**: a role ping naming exactly the same idols and groups as the
   person's open set (rule 6) joins that set instead of opening another.
5. **Reply** by the same person to a message that opened a set in the last
   hour: joins that set. Not remembered across a goyangi restart.
6. **Follow-up**: a message with media and none of the above, by the same
   person in the same channel, posted within 5 minutes of the last message
   that joined their open set: joins it. At most 3 follow-ups per set,
   counting repeat pings; a follow-up whose items all fail doesn't use one. A
   set opens this window when it's created from a ping, a mention or text
   detection, at least one item was saved, and the person is known (below).
   Replies join without moving the window.
7. **Text detection**, the last resort: the message names an idol goyangi
   knows. Strict on purpose: whole words only, names and aliases of 3+
   characters, links and `key: value` lines aren't read, an idol is required,
   and an idol name shared by several groups needs one of those groups named
   too. Every hit is logged for review.

**"The same person"** is the same Discord account: the author, the person who
ran an app command that produced the message, the `relay.user.id` you send,
or a relay's "Posted by:" name. An `uploader:` line changes who's credited,
not whose message it is. Posts that can't be tied to an account — another
bot's post that names nobody, or `relay.anonymous` — never join or open a set
across messages.

### `key: value` lines

Lines in the message text, one per line. The value runs to the end of the line
— don't add comments after it. They're read on a message that starts a set
(role ping, mention, text detection), and ignored on replies and follow-ups,
which take the set's.

| Key | Meaning |
|---|---|
| `idol:` | Idol names, comma-separated. |
| `group:` | Group names, comma-separated. |
| `tags:` | Tags, comma-separated. New tags are created. |
| `title:` | The post title. Default: `Idol - Group`, from the names that matched. |
| `date:` | `YYMMDD` (e.g. `240115`), `today` or `now`. Use `YYMMDD`: it also dates the set's title. Default: when goyangi archives it. |
| `source:` | An `http(s)` link to the source. Default: the first YouTube link in the message. Not a link → ignored. |
| `uploader:` | One name to credit, instead of the poster. |
| `filetype:` | `video` or `sticker`. (`image` and `gif` are decided from the file itself.) An unknown value makes every item fail. Video files are stored as looping clips (`gif`) unless this says `video`. |
| `mirror:` | An `http(s)` link credited as a mirror. Never downloaded. |

With a role ping, the lines add to or override what the roles say.

### Media

- **Attachments:** every attachment is attempted. Images and videos work; other
  files fail.
- **Links in the text** (written with `http://` or `https://`):
  - imgur posts (`imgur.com/…`, `i.imgur.com/…`), and imgur albums and
    galleries — every item in them;
  - `files.catbox.moe` file links;
  - `pixeldrain.com/u/…` and `/api/file/…` links;
  - any other link ending in `.mp4 .webm .mov .webp .avif .jpg .jpeg .png .gif`
    (any case, query string fine).
- **Not taken:** links to goyangi itself, media only in embeds, Discord
  stickers, forwarded messages, polls.
- **Limits:** 100 MB and 3 minutes per file; files fetched one after another.
  Stored formats: JPEG, PNG, WebP, AVIF, GIF, MP4, WebM, MKV, MOV.

### Credit

Who a post is credited to, first match wins:

1. An `uploader:` line.
2. `relay` (section 8).
3. The person who ran an app command that produced the message
   (`interaction_metadata.user`, or the older `interaction.user`).
4. For a bot or webhook author only: a `Posted by: name` line in the text or an
   embed's description, fields or footer — if the name matches an existing
   goyangi uploader, case-insensitively (as written, or with emoji and symbols
   stripped).
5. `author.username` — the username, not the display name. For a webhook,
   that's the name it posted under.

Names match an uploader's name or any of its aliases, case-insensitively. A
name that matches nobody gets a new uploader on first sight (except "Posted
by:" names, which only ever match). An uploader who turned off automatic
import, or was blocked, makes the message `skipped`.

### Sets and dates

Every message that isn't a reply, follow-up or repeat ping opens its own set,
even with one item (posts marked `filetype: sticker` get none). Each post
stores the message's link (`https://discord.com/channels/<guild>/<channel>/<message>`),
which is how duplicates are caught. If nothing in a message could be saved, no
empty set is left behind. Posts are dated when goyangi archives them unless a
`date:` line says otherwise.

## 13. Privacy

- **Stored, per post:** the media, the message's link, the attachment's original
  filename, the credited uploader name, the idols, groups and tags, and the
  title, source and mirror links. Not the message text itself. A new uploader
  or tag record can be created even when nothing ends up archived.
- **Logged:** goyangi's logs keep the message link, which names matched and,
  for failed items, the filename and error — which can include the attachment
  URL (signed, and expiring within a day). The server's output can also show
  item URLs, role names, credited names, and a `source:`/`mirror:` line that
  wasn't a link.
- **Discord user ids** (authors, `relay.user.id`) are held in memory for up to an
  hour to match follow-ups, and never stored.
- Forward only the agreed channels. Never DMs.
- **Deleted Discord messages stay archived**: goyangi doesn't follow deletes.
  Removal requests go through <https://goyangi.pics/takedown> or
  goyangi.pics@protonmail.com. Don't resend old messages: a post removed on
  request would be archived again by a resend.
- **Anonymous posts** are credited to the person who made them (section 8);
  your anonymous feature should say so.

## 14. Operations

- **Is goyangi up?** `GET https://api.goyangi.pics/api/health` → 200.
- **Token rotation.** goyangi accepts several tokens at once while switching:
  goyangi's admin adds the new one, you switch, the old one is removed. Your
  requests failing with 401 out of the blue means the token changed: ask.
- **Your bot's id** is on file on goyangi's side. A new bot (a new application)
  needs the admin to add its id first.
- **Changes to this contract** are additive only: new fields, statuses or
  reason codes. Ignore fields you don't know, and treat unknown statuses as
  "no reaction". A breaking change would come with a new path, agreed in
  advance.
- **Logging on your side:** the message id, status, reason, HTTP code and any
  `warnings` are enough. Never log the token.

## 15. Troubleshooting

| Symptom | Likely cause |
|---|---|
| Everything comes back `ignored` / `channel_not_listed` | The channel ids don't match the list — or the messages are in threads. |
| Follow-ups and replies come back `ignored` / `no_trigger` | They reached goyangi before their opener: send each channel's messages in order, one at a time (section 7). |
| Role pings come back `ignored` / `no_trigger`, with `warnings` | Your role cache missed, so goyangi got no role names. Fetch roles when the cache misses. |
| Role pings come back `ignored` / `no_trigger`, no warnings | The role names aren't in the `Idol [Group]` form. |
| Role pings come back `failed` / `unresolved_names` | The names parse, but goyangi doesn't know that idol or group yet — tell goyangi's admin. |
| `skipped` / `reannouncement` you didn't expect | The message @-mentions your bot and pings a role. |
| Posts your bot made for people are missing | Send them with `relay` (section 8); without it your bot's posts are ignored. |
| Posts credited to your bot's or a webhook's name | They were sent with `relay.anonymous`, or without `relay` from a webhook. |
| 400 "`bot_user_id` …" | Missing, or not the id goyangi's admin has on file. |
| 400 "Body must be JSON …" | Read the rest of the message: it names the field of the wrong type, e.g. a numeric id. |
| 400 "`message.guild_id` is required" | A message fetched over REST: add `guild_id`. |
| 400 about an attachment not on Discord's CDN | Attachment URLs must be sent exactly as Discord gave them. |
| 400 "`relay` is only for messages your bot posted" | `relay` on someone else's message, or `bot_user_id` is wrong. |
| Dry runs come back `duplicate` during the shadow | goyangi's own bot got there first. Compare the preview. |
| 401 | The token is wrong or was rotated. |
| 404 on POST | The API is switched off on goyangi's side. |
| 503 | goyangi is restarting or busy: wait `Retry-After` and resend. |

## 16. Checklist

- [ ] Message Content intent on, in the developer portal and in code.
- [ ] View Channel, Read Message History and Add Reactions in the agreed channels.
- [ ] Forward `MESSAGE_CREATE` only, from the agreed channels, with an attachment or `http` in the text.
- [ ] One lane per channel: the next message only after the previous POST has answered.
- [ ] Send the raw message object unmodified, ids as strings.
- [ ] Send a name for every id in `mention_roles`, fetching roles when the cache misses.
- [ ] Send `bot_user_id` — the id goyangi has on file — with every request.
- [ ] Posts your bot makes for people: once each, with `relay.user` (anonymous ones too), and not from the general forwarder.
- [ ] HTTP timeout 30–40 seconds.
- [ ] Retry timeouts, 5xx, 429 and 503 with backoff for at least 30 minutes; never retry 400, 401, 413 or a 404 on POST.
- [ ] After a 202, poll the GET for up to an hour; on a 404 there, resend the POST.
- [ ] React from `reaction`: `success` → ✅, `skipped` → ⏭️, anything else → nothing.
- [ ] Never let a failed request crash the bot. Never log the token.
- [ ] Optional: "Archive on goyangi" (section 9).
- [ ] Shadow-run with `"dry_run": true` before going live.
