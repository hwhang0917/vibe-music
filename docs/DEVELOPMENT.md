# Developing vibe-music

Technical reference for people working on the code. For using the app, see
the [README](../README.md).

## Architecture

```
                 ┌──────────────────────────────────┐
 guests ─votes──▶│  vibe-music (one Go binary)      │
 (browser)       │  admin window   : Wails          │
                 │  guest API + UI : Chi + SSE      │
                 │  core           : queue, votes,  │
                 │                   poll loop      │
                 │  sources        : local │ spotify │ youtube
                 └──────┬──────────┬──────────┬─────┘
                        ▼          ▼          ▼
                    speaker    Spotify     YouTube IFrame
                   (beep/oto)  desktop     player in the
                               (Web API)   admin window
```

- **Core owns the queue** (`internal/player`). Sorted by admin rank, then
  votes, then request time. The same song can be requested more than once.
  Skip fires when a configurable share of connected guests votes (default
  50%). The admin can always skip, reorder, remove, seek. Track end is
  detected by polling each source's `Status` once a second.
- **Enabled sources, exclusive Spotify.** Several sources can be on at once;
  the player switches source track by track. Spotify's developer policy
  forbids mixing its content with other audio, so it is marked exclusive:
  turning it on turns the others off and vice versa, with a confirmation when
  queued songs would be dropped.
- **Spotify is a remote control** (`internal/source/spotify`). Playback runs in
  the Spotify desktop client on the host; we only send Web API player
  commands (non-streaming app). PKCE with a loopback redirect on a fixed port
  (default 27272; Spotify rejects a port-less loopback URI). Refresh tokens
  rotate, so `savingTokenSource` persists every new one. Repeat is switched
  off on activation because repeat-track would defeat end detection.
- **YouTube is the official embedded player** (`internal/source/youtube`).
  Search and charts go through the Data API v3 with the host's key; playback
  is the IFrame player in the admin window, framed from a loopback http page
  Go serves (`player.html`) because YouTube refuses embeds without a Referer
  (error 153) and the `wails://` origin on Linux/macOS sends none. The page is
  addressed as `http://localhost:<port>`, never by IP: YouTube answers "This
  video is unavailable" for licensed music when the origin is an IP literal
  (verified with Firefox and WebKitGTK; `localhost` passes). Go drives
  it through Wails events (`yt:cmd`), relayed to the frame as postMessage, and
  state comes back through the `YouTubeReport` binding.
  No audio is fetched or decoded by vibe-music.
- **Local files** (`internal/source/local`). Folder scan with a 4-way worker
  pool; tags via `dhowden/tag`, duration via the beep decoders (MP3 needs a
  pass over the whole file); results cached in SQLite by path, size and mtime.
  Playback via beep/oto at a fixed 48 kHz with resampling; artwork read from
  the file on demand.
- **Guests** are a random cookie plus a display name. The database keeps the
  cookie's SHA-256 only. Invitation-only mode: single-use links with a TTL;
  redemption is a POST from the guest page rather than the link's GET so chat
  link previewers cannot spend the code.
- **Persistence** (`internal/store`): SQLite via `modernc.org/sqlite` (pure Go).
  Tables: settings, guests, invitations, queue, plays, local_index.

## Requirements

- Go 1.25+, Node.js 20+
- [Wails v2 CLI](https://wails.io/docs/gettingstarted/installation):
  `go install github.com/wailsapp/wails/v2/cmd/wails@latest`
- **Windows / macOS:** no C toolchain needed (`CGO_ENABLED=0` builds work).
- **Linux:** Wails needs WebKitGTK and the local player needs ALSA headers
  (`libasound2-dev`), both via cgo. Ubuntu 24.04 and Arch ship WebKitGTK 4.1
  only: build with `-tags webkit2_41` (`make build` detects it via pkg-config;
  CI passes it explicitly). See `wails doctor`.

## Develop, test, build

```sh
make install     # Go modules, Wails CLI, both frontends
make dev         # admin window with hot reload
make dev-web     # guest UI on :5173, proxies /api to the running guest server
make test        # go test (our packages only; node_modules ships Go files)
make lint        # gofmt + go vet + vue-tsc for both UIs
make build           # this machine: both UIs embedded, output in build/bin/
make build-windows   # CGO-free Windows .exe from any OS
```

`make help` lists every target. `wails.json` chains the guest UI build into
`frontend:build`, so `wails build` alone also produces the whole binary. Both
UIs are `go:embed`ed, so they must be built before anything Go compiles.

CI (`.github/workflows/ci.yml`) runs on every push and PR: build both UIs,
`make lint`, `make test`, then cross-builds the Windows exe and attaches it to
the run as a short-lived artifact.

### Releasing

`VERSION` holds the version (e.g. `0.0.1`); `version.go` embeds it, so the
binary, the admin Info dialog and the guest About dialog all show the same
value with no build flags. To release: bump `VERSION` on a branch and merge
it to `master`. `.github/workflows/release.yml` runs on pushes to `master`
that touch `VERSION`; if no tag `v<VERSION>` exists yet it builds
windows/amd64 and linux/amd64 (Ubuntu, Windows cross-compiled) and
darwin/amd64 (macOS runner), then creates the tag and a GitHub release with
generated notes. Pushing to other branches never releases. ARM targets are not
built yet.

## Layout

```
main.go, app.go            Wails app: bindings, config, guest server lifecycle
logging.go                 JSON log file in the data dir; std log routed into it
internal/source/           Source interface, Track, optional Charter/Browser/ArtworkProvider
internal/source/local/     folder scan + index cache, tags, search, browse, beep playback
internal/source/spotify/   PKCE connect, Web API remote control, search/browse, end detection
internal/source/youtube/   Data API search/chart/channel, embedded IFrame player control
internal/source/fake/      in-memory source for tests
internal/player/           queue, votes, skip threshold, poll loop, state fan-out
internal/config/           settings (in the store), sealed secret files
internal/store/            SQLite: settings, guests, invitations, queue, plays, local index
internal/server/           Chi: guest cookie + name, /api/*, SSE, SPA fallback, request log
ui/theme.css               DESIGN.md tokens mapped onto shadcn-vue's CSS variables
ui/i18n.ts                 framework-free EN/KO lookup shared by both UIs
ui/attributions.ts         open-source list shown by both UIs (direct deps; update when adding one)
frontend/                  admin UI (Vue + Vite + Tailwind + shadcn-vue), embedded by Wails
web/                       guest UI (Vue + Vite + Tailwind + shadcn-vue + vue-router), embedded via web/embed.go
```

### Guest API

| Method | Path | Notes |
|---|---|---|
| GET | `/api/health` | liveness for the offline ribbon |
| POST | `/api/join` `{code}` | redeem an invitation (before the guest gate) |
| GET | `/api/state`, `/api/events` | snapshot, SSE stream (`state` events, `: ping` every 30 s) |
| GET/POST | `/api/me` | guest name |
| GET | `/api/search?q&source` | 20 results from one enabled source |
| GET | `/api/top?source` | 10 most played here (from `plays`) |
| GET | `/api/chart?source&region` | Top 50 (`source.Charter`) |
| GET | `/api/artist?source&id`, `/api/album?source&id` | browse pages (`source.Browser`) |
| GET | `/api/folder?source&id` | one level of the folder tree, empty id = top (`source.Explorer`) |
| GET | `/api/artwork/{source}/{id}` | local artwork |
| POST | `/api/queue`, `/api/queue/{id}/vote`, `/api/skip`; DELETE `/api/queue/{id}` | need a name |

Errors are `{"error": "<code>"}`; both UIs translate codes in `i18n.ts`
(`err.<code>`), with `code: detail` carrying a provider's own message.

### UI stack

Both apps use [shadcn-vue](https://www.shadcn-vue.com/) components copied into
`src/components/ui/` (reka-ui primitives, Tailwind v4, lucide icons). Add more
with `cd web && npx shadcn-vue@latest add dialog` (or `frontend/`), or copy a
component folder from the other app; both share one shadcn setup.

`ui/theme.css` maps DESIGN.md's Geist tokens onto shadcn's variables, so
components pick up the ink/hairline look without per-component overrides.
TypeScript is pinned to 5.x in both apps: Vue's SFC compiler needs the TS 5
JavaScript API to resolve the imported prop types shadcn components use, and
the TypeScript 7 package does not ship it.

The admin's `wailsjs/go/main/App.{js,d.ts}` bindings are hand-written to match
`app.go`; add a line there when adding a binding. The guest app keeps all
shared state and the SSE stream in `web/src/state.ts`; pages are route
components under `web/src/`.

### Adding a source

Implement `source.Source` in `internal/source/<name>/`, add it to the
`Sources` list in `app.go`, and give it a card in the admin UI. Optional
interfaces: `source.ArtworkProvider` if artwork is not a public URL,
`source.Charter` for a Top chart, `source.Browser` for artist and album pages
(set `ArtistID`/`AlbumID` on tracks so the guest UI knows what is linkable).
Return `*source.CodedError` sentinels for failures the UIs should translate.

## Storage and security

Data lives in `$XDG_CONFIG_HOME/vibe-music/` (Linux),
`~/Library/Application Support/vibe-music/` (macOS), or `%AppData%\vibe-music\`
(Windows); `VIBE_MUSIC_DIR` overrides it.

- `vibe-music.db`: settings, guests (cookie SHA-256, name, blocked, admitted),
  invitations (code SHA-256, label, expiry, revoked, used_by), queue, plays,
  local_index. `0600`.
- `vibe-music.log`: JSON lines. One `http` line per request (method, path,
  status, bytes, ms, ip, ua), `guest` lines for name/request/vote/remove/skip/
  join, `admin` lines for every admin action. Rotated once at 5 MB. `0600`
  because it carries names and IPs.
- `spotify-token.json`, `youtube-api-key`: sealed at rest. Windows uses DPAPI
  (keyed by the logged-in user's credentials, nothing stored beside the file);
  other platforms AES-256-GCM with a random key in `secret.key` next to the
  data (guards a copied file, not the same account; OS keychain is the upgrade).
  Plaintext files from older builds are sealed on first read.
- No bcrypt: there are no passwords. Every secret is a high-entropy random
  token, where a fast hash (or sealing) is the right tool.
- Guest identity and invitation codes are stored hashed; the plaintext code is
  shown in the admin only for codes created since launch.

## Spotify policy notes

- Every Spotify track shown to guests carries the Spotify mark and a
  "Listen on Spotify" link. Check the marks in `web/src/SourceIcon.vue`
  against the official brand kits before shipping.
- Spotify is licensed for personal, non-commercial use.
- Spotify's editorial charts are not available to development-mode apps, so
  there is no Spotify Top 50; YouTube's chart comes from `videos.list
  chart=mostPopular`.
- Artist pages use `country=from_token` on the top-tracks endpoint, the same
  market value Search uses.
