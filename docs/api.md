# HTTP API

Base address (local): `http://127.0.0.1:8080`. All responses are JSON
(`Content-Type: application/json`) and carry the security headers listed in
[security.md](security.md#http-security-headers).

There's no authentication yet. The server only listens on `127.0.0.1`, so only this machine
can reach it. Authentication arrives with the friends-sharing weeks.

Unknown paths return **404**. A known path with the wrong method returns **405** with an
`Allow` header (for example `Allow: GET, HEAD`). Both come from Go's `ServeMux`, with a
plain-text body. Every `GET` route also answers `HEAD` (same headers, no body).

## Errors

Errors use one shape:

```json
{"error": "human-readable message"}
```

- **4xx**: the request is wrong; retrying it unchanged won't help.
- **5xx**: the server failed; the body is always the generic `{"error":"internal error"}`.
  The real cause is only in the server log, never in the response.

---

## `GET /healthz`: liveness

Is the process alive? Never touches the database.

```
200 {"status":"ok"}
```

## `GET /readyz`: readiness

Can the server handle requests right now? Pings Postgres with a 2-second timeout. (Object
storage is checked once, at startup: the API refuses to start without it.)

```
200 {"status":"ready"}
503 {"status":"unavailable"}     database unreachable (cause logged as [NOT_READY])
```

## `GET /api/v1/tracks`: list tracks

Newest first (`created_at DESC`, ties broken by `id`).

| Query parameter | Default | Allowed |
| --- | --- | --- |
| `limit` | 50 | integer, 1–200 |

```
200 {"tracks":[{
      "id": "02c2eaad-a2cf-4dca-ad54-b765ccc73201",
      "title": "First",            string or null (null until tags are read)
      "artist": "Someone",         string or null
      "album": null,               string or null
      "status": "pending",         "pending" | "ready" | "failed"
      "size_bytes": 1000,
      "created_at": "2026-10-25T11:32:04.445222+01:00"
    }]}
400 {"error":"limit must be an integer between 1 and 200"}
500 {"error":"internal error"}
```

An empty library returns `{"tracks":[]}`, never `null`.

Not exposed on purpose: `sha256` (clients don't need it, and every field sent becomes a field
that can't be removed without breaking clients).

Not yet: pagination (a cursor for "the next page"). The `limit` cap keeps responses bounded until then.

## `POST /api/v1/tracks`: upload a track

The request body is the raw audio file, not a multipart form:

```sh
curl -i --data-binary @song.flac localhost:8080/api/v1/tracks
```

```
201 {"track":{"id":"…","title":null,"artist":null,"album":null,
              "status":"pending","size_bytes":5242880,"created_at":"…"}}     new track
200 {"track":{…}}                       the identical file is already in the library (nothing stored twice)
400 {"error":"request body is empty"}
400 {"error":"upload interrupted"}      the client disconnected or was too slow (10-minute limit)
413 {"error":"file is larger than 200 MiB"}
415 {"error":"unsupported file type: send an MP3, FLAC, Ogg, WAV or M4A file as the raw request body"}
500 {"error":"internal error"}
```

- **Type** is detected from the file's first bytes (MP3, FLAC, Ogg, WAV, M4A). The request's
  `Content-Type` is ignored. Passing the header check doesn't make a file valid: the ingest
  worker checks the whole file and can still mark the track `failed`.
- **Duplicates** are recognised by the SHA-256 the server computes from the bytes. Uploading the
  same file again returns the existing track with 200.
- The track starts as `pending`. Title, artist and album stay `null` until the ingest worker
  reads the tags.
- Not yet: `GET /api/v1/tracks/{id}` (so there's no `Location` header), and recording who uploaded
  the track (comes with authentication).
