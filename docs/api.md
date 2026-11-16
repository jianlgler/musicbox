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
      "album_artist": null,        string or null
      "track_number": 3,           integer or null
      "disc_number": null,         integer or null
      "year": 2024,                integer or null
      "duration_ms": 215000,       integer or null (null until the worker has run)
      "has_cover": true,           whether GET /api/v1/tracks/{id}/cover has an image
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
- The track starts as `pending`. The tag fields stay `null` until the ingest worker
  reads the tags.
- Not yet: a `Location` header pointing at `GET /api/v1/tracks/{id}`, and recording who uploaded
  the track (comes with authentication).

## `GET /api/v1/tracks/{id}`: one track

Any status (`pending`, `ready`, `failed`), so a client can follow an upload.

```
200 {"track":{…same fields as in the list…}}
404 {"error":"track not found"}     unknown id, or not a UUID at all
500 {"error":"internal error"}
```

A malformed id (`/tracks/hello`) is a 404 decided in Go: it never reaches Postgres, which would
answer it with an error.

## `GET /api/v1/tracks/{id}/cover`: cover art

The image itself, not JSON.

```
200 <image bytes>    Content-Type: image/jpeg or image/png
                     Cache-Control: private, max-age=86400
404 {"error":"track not found"}
404 {"error":"track has no cover"}       has_cover is false
500 {"error":"internal error"}
```

- `Content-Type` comes from the image's first bytes, checked again on every request. Only JPEG
  and PNG exist (ingest drops anything else).
- It's the only response that may be cached: a track's cover can't change, because the file
  can't. `private` keeps it out of shared caches. Errors stay `no-store`.
- Tracks of one album usually share one image, stored once. For an album, use its
  `cover_track_id`.

---

## Library: artists, albums, search

These only include `ready` tracks. There are no artist or album tables: both are built from the
tags.

- **An album** is the pair (album artist, album title). The album artist is the track's
  `album_artist` tag, or its `artist` when that's missing. A compilation (one album artist,
  many track artists) stays one album; two albums both called "Greatest Hits" stay two.
- **An artist** is an album artist in that sense, so every listed artist has their albums under
  `GET /api/v1/albums?artist=…`. A guest on a compilation track isn't listed as an artist; search
  still finds the track.
- Tracks without an album, or without any artist, appear in no album; search finds them.
- Names are matched **exactly** (case and accents included). Take them from the lists, don't
  type them.
- Names go in the query string, percent-encoded (`AC%2FDC`). They can hold `/`, `&`, `?` and
  anything else.

Text parameters (`artist`, `album`, `q`) are trimmed, and rejected with 400 when they're:

| Problem | Error |
| --- | --- |
| missing or blank, where required | `{"error":"q is required"}` |
| too long (500 characters for names, 100 for `q`; characters, not bytes) | `{"error":"artist must be at most 500 characters"}` |
| given twice (`?q=a&q=b`) | `{"error":"q must be given once"}` |
| invalid UTF-8 or a NUL byte (`%FF`, `%00`) | `{"error":"q must be valid UTF-8 text"}` |

`limit` works as on `GET /api/v1/tracks` (default 50, 1–200). Empty results are `[]`, never `null`.

### `GET /api/v1/artists`

Alphabetical, ignoring case.

```
200 {"artists":[{"name":"Shared Cover Band","album_count":1,"track_count":2}]}
```

`album_count` can be 0: an artist whose tracks have no album tag.

### `GET /api/v1/albums`

Alphabetical by artist, then title. `?artist=NAME` keeps one artist's albums.

```
200 {"albums":[{
      "title": "Waveforms",
      "artist": "Various",                                  the album artist
      "year": 2024,                                         latest year among its tracks, or null
      "track_count": 4,
      "duration_ms": 1060,                                  sum; tracks of unknown length count as 0
      "cover_track_id": "1bb64500-5ec1-42ca-8b53-1ee466b615b5"   first track with a cover, or null
    }]}
```

### `GET /api/v1/albums/tracks?artist=NAME&album=TITLE`

One album and its tracks in play order: disc, then track number (missing numbers last), then
title. At most 1000 tracks.

```
200 {"album":{…as above…},"tracks":[{…track fields…}]}
400 {"error":"album is required"}
404 {"error":"album not found"}
```

### `GET /api/v1/search?q=TEXT`

Tracks whose title, artist, album or album artist **contains** `q`, ignoring case. Title
matches come first, then alphabetical by title.

```
200 {"tracks":[{…track fields…}]}
400 {"error":"q is required"}
```

`%` and `_` are searched for literally: `q=100%` finds "100% Pure", not every track.

Not yet: paging past `limit` (a cursor), accent-insensitive search ("Beyonce" finding
"Beyoncé"), and ranking by relevance.
