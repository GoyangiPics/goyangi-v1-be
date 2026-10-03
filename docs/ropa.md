# Record of Processing Activities (GDPR Art. 30)

Controller: **Goyangi** — private individual (non-commercial hobby project).
Contact: goyangi.pics@protonmail.com · No DPO (not required — Art. 37 thresholds not met).
Scope: goyangi.pics website (Nuxt frontend + PocketBase backend) and the Goyangi Discord bot.
Last reviewed: 2026-09-01.

> Kept current alongside code changes. If a new feature stores personal data,
> it gets a row here and a section in the site's `/privacy` page in the same PR.

| # | Activity | Data subjects | Data categories | Legal basis | Recipients / processors | Transfers | Retention | Security |
|---|---|---|---|---|---|---|---|---|
| 1 | Account management | Registered users | Email, password hash, avatar; OAuth link (Discord id, username, email) for OAuth sign-ins | Contract (6(1)(b)) | Self-hosted PocketBase; Proton (mail) | None <!-- TODO(operator): confirm hosting + region --> | Until account deletion (self-service) | HTTPS, hashed passwords, superuser-only admin |
| 2 | Content hosting & attribution | Registered users (uploaders) | Chosen uploader display name + aliases, shown publicly on content | Contract (6(1)(b)) | Cloudflare R2 (media, EU region, standard DPA) | Cloudflare DPA/SCCs | Until content removed; credit anonymized on account deletion (`hooks/users.go`) | Public by design (name only) |
| 3 | Discord bot ingestion | Discord posters in allowlisted channels (members and non-members) | Media files; Discord username as public credit (`uploaders`); message jump link (`contents.discord`). Message text parsed transiently, never stored; author id in-memory only (1h chain TTL) | Legitimate interests (6(1)(f)) — attributable fan archive; pinned channel notice + opt-out | Cloudflare R2; Discord (source platform) | Cloudflare DPA/SCCs | As row 2; erasure on request via takedown page / email | Allowlist-gated triggers; role-gated manual paths |
| 4 | Activity records | Registered users | Likes, stars, filters, labels applied, collections, reports, links-tool entries (public) | Contract (6(1)(b)) | Self-hosted PocketBase | None | Until account deletion (cascade or `hooks/users.go`) | Owner-scoped API rules (links are public) |
| 5 | Support & data-request mailbox | Anyone who writes in | Email address, request content | Legitimate interests (6(1)(f)) / legal obligation for DSRs (6(1)(c)) | Proton Mail | Proton (CH — adequacy) | Duration of the matter | Mailbox MFA |
| 6 | Security & operational logs | All visitors; Discord posters | PocketBase request logs (IP; `logs.maxDays` ≤ 7, verify in prod); `system_logs` (errors/warnings, derived data only — no message text since 2026-09); stdout/journal on the host (channel/set ids; no author ids since 2026-09) | Legitimate interests (6(1)(f)) — abuse prevention, debugging | Self-hosted host journal | None | Request logs ≤ 7 days; `system_logs` 90 days (retention cron, `GOYANGI_LOG_RETENTION_DAYS`); journal per journald config <!-- TODO(operator): set a journald size/time cap --> | Superuser-only collection |
| 7 | Backups | All of the above | Full database snapshots | Legitimate interests (6(1)(f)) — service continuity | Backup destination <!-- TODO(operator): destination + retention window --> | TODO | Rolling window — deleted data ages out | Encrypted at rest <!-- TODO(operator): confirm --> |

**Not processed:** special categories (Art. 9), children's data (18+ service), payment data, analytics/tracking of any kind. No automated decision-making or profiling. Data is never sold and never used for AI/ML training.

**Breach log:** kept below this line as incidents occur (Art. 33(5) requires documenting every breach, reported or not).

_No incidents recorded._
