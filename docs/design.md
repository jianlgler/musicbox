# Design: musicbox

| | |
| --- | --- |
| Status | Week 1 complete: API, Postgres, object storage. Later sections describe planned work and are marked as such |
| Related | [architecture.md](architecture.md) (components and layers), [security.md](security.md) (measures in detail) |

## 1. Summary

musicbox is a self-hosted music server for a small, invite-only group of friends. One person
runs it; friends listen through desktop, Android and web clients, add music to a shared
library, and keep their own playlists. It doubles as a learning project for system design and
DevSecOps, so some choices favor learning value, and those are called out.

## 2. Goals and non-goals

### Goals

1. Stream a personal music library to a few people on desktop, Android and web.
2. Add music two ways: an upload endpoint, and a folder-import CLI (`musicctl import`).
3. Invite-only sharing: roles, invites, private and shared playlists.
4. Runs on one machine with Docker Compose, and moves to AWS (later, a NAS) by changing
   configuration, not code.
5. Secure against a malicious *member*, not only outsiders (see [threat model](#7-threat-model)).
6. Every part observable, tested, and deployable through a CI pipeline.

### Non-goals

Writing these down stops scope creep.

- **No public sign-up.** Accounts exist only through invites.
- **No music discovery, recommendations or social feed.**
- **No large scale.** Designed for tens of users, not thousands (see [capacity](#5-capacity-assumptions)).
- **No DRM, and no store or marketplace.** It plays files the group owns.
- **No offline sync in v1.** Clients stream; downloading for offline use may come later.
- **No high availability in v1.** A few minutes of downtime for an upgrade is acceptable.

## 3. Users and roles (planned)

| Role | Can |
| --- | --- |
| **Owner** | Everything: manage members and invites, delete any track, see server health |
| **Member** | Stream the shared library, add tracks, manage their own playlists, share playlists with other members |

There's no anonymous role: every request except `/healthz` and `/readyz` will require a session.

## 4. Key flows

### Listing tracks (built)

```
client → GET /api/v1/tracks?limit=N → validate → SELECT … ORDER BY created_at DESC, id LIMIT N → JSON
```

### Adding a track (upload built; worker planned)

```
1. upload or musicctl import
2. hash the file while reading it (SHA-256)
3. PUT s3://musicbox/audio/<sha256>                 ← same file twice = same key, stored once
4. INSERT INTO tracks (sha256, size_bytes, status='pending')
   ON CONFLICT (sha256) DO NOTHING                 ← idempotent: re-imports are harmless
5. enqueue an ingest job (Postgres queue, SELECT … FOR UPDATE SKIP LOCKED)
6. ingest worker: read tags (title, artist, album), validate the audio, set status='ready'
   or 'failed'
```

Steps 3–4 can't be one transaction, because storage and Postgres are separate systems. The
order (store the object first, then the row) means a crash leaves at worst an orphan object,
never a row pointing at missing audio. A cleanup job can remove orphans.

### Playback (planned)

```
1. client asks the API for a stream URL for track X
2. API checks the member may play it, returns a short-lived stream token
3. <audio src="…/stream/X?token=…">               ← HTML audio can't send an Authorization header
4. streaming service checks the token, reads byte ranges from storage, serves them
```

Whether the streaming service **proxies** the bytes or redirects to a **presigned storage URL**
is an [open question](#9-open-questions) (W22).

## 5. Capacity assumptions

Rough numbers, so design choices have a basis. They're assumptions, not measurements.

| Quantity | Assumption | Consequence |
| --- | --- | --- |
| Members | ≤ 20 | One Postgres, no sharding; a connection pool of a few connections is enough |
| Library | 20,000 tracks × ~8 MB | ~160 GB of audio: fine for one disk or bucket; far too much for Postgres rows |
| Metadata | 20,000 rows × ~1 KB | ~20 MB: Postgres keeps it all in memory |
| Concurrent streams | ≤ 5 × 320 kbit/s | ~1.6 Mbit/s: a home upload link handles it |
| Imports | bursts of thousands of files | Needs a queue and idempotent jobs, not synchronous processing |

## 6. Data model

### Built

`tracks`: one row per audio file, keyed by a random UUID, with a unique SHA-256 linking it to
its object in storage. Details in [database.md](database.md).

### Planned

| Table | Purpose | Key rules |
| --- | --- | --- |
| `users` | members and the owner | role is `owner` or `member`; passwords hashed with a slow hash (argon2id or bcrypt) |
| `invites` | single-use invite codes | stored hashed, expire, one use each |
| `playlists` | a member's playlists | `owner_id`, `visibility` (`private` / `shared`) |
| `playlist_tracks` | tracks in a playlist, ordered | primary key `(playlist_id, position)` |
| `jobs` | the work queue for ingest | status, attempts, lease expiry, idempotency key |
| `tracks.added_by` | who added a track | for accountability and the owner's moderation |

Every query on a member's objects includes the owner check
(`WHERE id = $1 AND owner_id = $2`), and returns 404, not 403, when it fails, so object
existence isn't revealed.

## 7. Threat model

### What we protect

| Asset | Why it matters |
| --- | --- |
| Audio library | Members' files; losing or leaking it is the main harm |
| Credentials | Database password, S3 keys, members' passwords, session and stream tokens |
| Private playlists | Members expect them to stay private, even from other members |
| Availability | The server is shared; one member shouldn't be able to take it down for everyone |
| The host machine | It's the owner's personal computer (later a NAS) |

### Who might attack

| Actor | Access | Example motive |
| --- | --- | --- |
| **Malicious or careless member** | Valid account | Read others' private playlists, delete others' tracks, upload malware disguised as audio, flood the server |
| **Compromised member device** | A member's valid tokens | Same as above, without the member knowing |
| **Outsider on the network** | Can reach exposed ports | Unauthenticated access, credential guessing |
| **Local software** | Runs on the host (malicious package, browser page via DNS rebinding) | Reach `127.0.0.1` services |
| **Supply chain** | Upstream images and Go modules | Get malicious code into the build |

### Trust boundaries

```
 member devices ──(1)── API / streaming ──(2)── Postgres
                                        └─(2)── SeaweedFS
 upstream images, Go modules ──(3)── build
```

1. **Client → server:** nothing from a client is trusted: input, IDs, file contents, tokens.
2. **Server → backing services:** authenticated with least-privilege credentials, reachable only locally.
3. **Third-party code → build:** pinned and reviewed before it's used.

### STRIDE

STRIDE is a checklist of six threat types. Status: ✅ in place, 🔜 planned.

| Threat | Example here | Mitigation | Status |
| --- | --- | --- | --- |
| **S**poofing (pretending to be someone) | A request to S3 without the real key | SigV4 signatures; no anonymous S3 identity | ✅ |
| | Logging in as another member | Hashed passwords, invite-only accounts, sessions or JWTs with short lifetimes | 🔜 |
| **T**ampering (changing data) | Altering a request to storage in transit | Signatures cover method, path, headers and time | ✅ |
| | Uploading a file whose content doesn't match its claimed hash | Server computes the SHA-256 itself; never trusts a client-supplied hash | ✅ |
| | Changing the schema half-way | Transactional migrations under an advisory lock | ✅ |
| **R**epudiation (denying an action) | A member denies deleting a track | Request logging; `tracks.added_by`; an audit log of destructive actions | partly ✅ (request log) |
| | Forged lines hiding an attacker's actions in logs | Client input logged with `%q` (no newlines); query strings never logged | ✅ |
| **I**nformation disclosure | Reading another member's private playlist by guessing its ID (IDOR) | Owner check on every object query, 404 on failure; random UUIDs | 🔜 (UUIDs ✅) |
| | Internal details in error messages | Generic 500 bodies; details only in server logs; tested | ✅ |
| | Secrets in the repo, logs or process list | `.env` and `s3.json` git-ignored and `600`; DSN never logged; keys via environment | ✅ |
| | Database or storage reachable from the network | Ports bound to `127.0.0.1`; storage admin ports not published | ✅ |
| | Stream tokens leaking through URLs | Short-lived, single-track tokens; `Referrer-Policy: no-referrer`; tokens never logged | partly ✅ |
| **D**enial of service | Huge list requests | `limit` capped at 200 | ✅ |
| | Slowloris connections | `ReadHeaderTimeout: 5s` | ✅ |
| | Uploading enormous or endless files | Upload size limit and read deadline ✅; per-member quota 🔜 | partly ✅ |
| | One member flooding the API | Rate limiting per member | 🔜 |
| | Crafted audio crashing the ingest worker | Worker isolated from the API; per-job timeouts; failures marked `failed`, not retried forever | 🔜 |
| **E**levation of privilege | A member doing owner actions | Role checks on owner-only endpoints | 🔜 |
| | A compromised API managing storage | S3 identity limited to Read/Write/List on one bucket; no Admin | ✅ |
| | SQL injection dropping tables | Parameterized queries ✅; separate migrator and app database roles 🔜 | partly ✅ |
| | Malicious upstream image | Images pinned by digest; adopted only when ≥ 2 weeks old | ✅ |

### Accepted risks (for now)

- **Plain HTTP on localhost.** Traffic never leaves the machine; TLS arrives with remote access.
- **The `docker` group is root-equivalent.** Anyone with Docker access on the host controls
  everything. Accepted on a single-user machine.
- **No backups yet.** A disk failure loses the library. To be addressed before friends add music.

## 8. Alternatives considered

| Decision | Chosen | Rejected | Why |
| --- | --- | --- | --- |
| Backend language | Go | Python | Learning goal; single static binary; strong standard HTTP library; good concurrency for streaming |
| Clients | React + TypeScript in Tauri | Native Kotlin + desktop app | One UI codebase for desktop, Android and web |
| HTTP routing | standard `net/http` | Gin, Echo, chi | Go 1.22+ routing is enough; fewer dependencies to vet |
| Job queue | Postgres table, `SKIP LOCKED` | Redis, Kafka, RabbitMQ | One less system to run and secure; jobs and data commit together; enough throughput at this scale |
| Audio storage | S3 object storage | Postgres `bytea`, local disk | Database stays small; shared by every instance; portable to AWS and the NAS |
| S3 server (local) | SeaweedFS | MinIO | MinIO community edition stopped shipping images (Oct 2025) and was archived; SeaweedFS is also the NAS plan |
| Migrations | own small runner | goose, golang-migrate | Learning (transactions, advisory locks, `go:embed`); easy to swap later |
| IDs | random UUIDs | auto-increment integers | Not enumerable; can be generated anywhere. Not a substitute for authorization |
| Streaming | separate service (from W19) | stays in the API | Long-lived connections and bandwidth scale differently from short API calls |
| Ingest entry points | upload endpoint + import CLI | a watched folder rescanned on a schedule | Explicit, idempotent, and testable; a watched folder may come later as a convenience |

## 9. Open questions

| Question | Options | Decide by |
| --- | --- | --- |
| How does the client get audio bytes? | Streaming service **proxies** them (full control, more load) vs **presigned URL** to storage (less load, storage must be reachable by clients) | W22 |
| Sessions or JWTs? | Server-side sessions (easy revocation) vs JWTs (stateless, harder to revoke) | auth weeks |
| Transcoding | Serve originals only vs transcode to a lower bitrate for mobile | after ingest works |
| Backups | What, how often, where (another disk, another provider), and how restores are tested | before sharing with friends |
| NAS migration | SeaweedFS on the NAS as primary storage vs as a replica | after the project is complete |
| Legal | Sharing ripped commercial music is likely not covered by private-copy exceptions; use Creative Commons music for the shared library | ongoing |
