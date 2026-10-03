# Data-Subject Request Runbook

How to handle GDPR requests for goyangi.pics. One person runs this; the point
of the runbook is that nothing depends on remembering the details under time
pressure.

**The clock: one month** from receiving a request (Art. 12(3)). Extendable by
two further months for complex cases, but only if the requester is told within
the first month, with reasons. Log every request and its outcome at the bottom
of this file.

**Identity:** verify only when there is reasonable doubt. A request sent from
the account's registered email, or made while logged in, needs no extra proof.
A Discord poster claiming an uploader credit proves control of the Discord
account — easiest is a DM exchange from that account, or a message in the
server. Never ask for ID documents at this scale.

## Access / export (Art. 15, 20)

- **Registered user:** point them at **Profile → Your data → Download** —
  self-service JSON of account, uploader, likes, stars, filters, links,
  collections and uploads list. Covers both access and portability.
- **By email / no longer able to log in:** admin UI → query each collection by
  their user id (`users_likes`, `users_stars`, `users_filters`, `users_links`,
  `contents_collections`, `uploaders`, then `contents` by uploader id), export
  as JSON, send to the registered email address only.
- **Discord-only poster:** the only personal data held is the `uploaders`
  record (name/aliases) and content credited to it — export those.

## Erasure (Art. 17)

- **Registered user:** self-service delete on Profile, or delete the `users`
  record in the admin UI — both fire the same `OnRecordDelete` hook
  (`hooks/users.go`), which anonymizes their uploader credit to
  `deleted-<suffix>`, clears aliases, deletes their public links rows and
  solely-owned collections. Cascades handle likes/stars/filters/reports.
- **Discord-only poster (no account):** admin UI → `uploaders` → their record:
  rename to `deleted-<suffix>`, clear `aliases` — the same treatment the hook
  applies. Set `blockIngest` if they also opt out of future archiving.
- **Content removal on request:** delete the `contents` records (R2 objects are
  cleaned up by the delete hooks). Content depicting the requester or owned by
  them: takedown page criteria apply — remove promptly.
- Backups: deleted data ages out with the backup retention window; note the
  request date in the log so a restore never resurrects erased data silently.

## Rectification / restriction / objection (Art. 16, 18, 21)

- Rectification: fix the field (admin UI) or tell the user where to self-serve
  (email is fixed via support; uploader name is self-service).
- Objection to Discord-ingestion credit: treat as erasure of the credit
  (anonymize) + `blockIngest` for the future. The legitimate-interest balance
  rarely favours keeping a name against its owner's objection — default to
  honouring it.

## Refusals

Only for manifestly unfounded or excessive/repetitive requests. Refuse within
one month, in writing, naming the reason and their right to complain to a
supervisory authority and to seek a judicial remedy.

## Breach procedure (Art. 33/34)

1. Contain: rotate the affected secrets (`DISCORD_TOKEN`, R2 keys, OAuth
   client secrets, superuser password), take the service down if needed.
2. Assess risk to individuals. Unless the breach is *unlikely* to risk anyone
   (e.g. already-public data only), **notify the supervisory authority within
   72 hours** of becoming aware, via its breach notification form. Late =
   explain the delay.
3. If bot/Discord data is affected, **notify Discord promptly**
   (Developer ToS §5(c)).
4. High risk to individuals (e.g. email+password-hash dump): notify the
   affected users directly.
5. Document the incident in `ropa.md`'s breach log **even if not reported**.

## Request log

| Date received | Who (account/email) | Request | Outcome | Date closed |
|---|---|---|---|---|
| _none yet_ | | | | |
