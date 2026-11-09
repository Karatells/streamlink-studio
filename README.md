# Streamlink Studio

A lightweight web panel that monitors channels, records live streams with
[`streamlink`](https://streamlink.github.io/), post-processes them with `ffmpeg`
and lets you play, convert and trim recordings in the browser.

* **Auto-monitoring & recording** – a background poller checks every channel and starts recording when it goes live.
* **Auth & plugins** – Twitch OAuth token (ad-free / sub-only), `cookies.txt` (YouTube members-only, 18+), HTTP proxy & headers (Kick), plugin e-mail/password. Global credentials with per-channel overrides.
* **Recordings library** – list grouped by day (Today / Yesterday / …) or by streamer, search, click a nick to filter by it, in-browser player (HTTP range seeking), download, delete.
* **Processing tab** – converting, trimming and merging jobs live on their own sub-tab of the Recordings page (with a running-jobs counter) instead of cluttering the list.
* **Notifications** – a bell in the top bar (unread counter) opens a page with the event log: streamer went live / ended the stream, recording or authorization problems, processing started / finished / failed, update available. New events also pop up as toasts. What to notify about can be switched per type on that page; the log keeps the last 300 events in `<data>/notifications.json`. Repeated identical errors are collapsed (1 h), and streams that are already live when Studio starts do not produce a "went live" event.
* **Gotify** – Settings → Notifications can forward events to a [Gotify](https://gotify.net) server (push to phone/desktop): server URL, application token (kept on the server, never sent back to the browser), message language and a checklist of which event types to send. Delivery is independent of the in-app mute list, retried once on transient errors, and the last success/error is shown next to a **Send test** button. Priority follows severity (info/success 4, warning 6, error 8).
* **Platform icons** – channels, recordings, statistics and notifications show a platform badge. Official logos are trademarks, so Studio ships neutral monogram badges (Tw, YT, K, …); to use real logos put files named after the platform into `<data>/icons/` (e.g. `twitch.svg`, `kick.png`, `youtube.webp`) – they are picked up without a restart.
* **Bulk actions** – tick several files (shift-click selects a range, a group checkbox selects a whole day) to delete them, **merge** them or move them to the **Saved** tab. Merging joins the files from the oldest to the newest start time with ffmpeg's concat demuxer and stream copy (no re-encoding; all files must share one format, optionally delete the originals afterwards).
* **Converter & trim editor** – lossless TS/MKV/FLV → MP4 remux; visual IN/OUT timeline with *Fast Cut* (lossless) or *Accurate Cut* (re-encode).
* **UI** – English / Українська / Русский (add more by dropping a JSON file), dark / light / auto theme.
* **Single static binary** – the UI is embedded with `go:embed`; state lives in one JSON file.

## Install (Linux, systemd)

```bash
curl -fsSL https://raw.githubusercontent.com/Karatells/streamlink-studio/main/install.sh | sudo bash
# non-interactive:
... | sudo bash -s -- --install --channel=stable --port=8080 --user=admin --password=change-me
... | sudo bash -s -- --update  --channel=beta
```

Channels: `stable` (latest release), `beta` (latest pre-release), `nightly` (built from `main`).
The script installs `ffmpeg`/`python3`, installs & upgrades `streamlink` in `/opt/streamlink-studio/venv`,
verifies the download checksum, swaps the binary atomically (keeping `.prev` for rollback) and
restarts the service. Updates never touch `/var/lib/streamlink-studio` (database, cookies, recordings).
`--uninstall [--purge]` removes it.

## Settings & updates

Settings are split into tabs (`#/settings/general|auth|tools|stats|updates`): **General** (defaults for new channels),
**Authorization** (Twitch token, global cookies, proxy), **Tools** (streamlink/ffmpeg paths and status), **Statistics** (disk usage and a forecast of when it fills up, library size, saved files, recordings per day for 14 days, space by streamer and platform, largest files, server load/memory/uptime; refreshes every 15 s) and **Updates**.

* **Update check** looks at the channel the program was installed from (`STUDIO_CHANNEL`, written by `install.sh`;
  inferred from the version if absent). You can pick another channel on the Updates tab; the installed one is marked ★.
  `stable` = latest release, `beta` = newest version incl. pre-releases, `nightly` = rolling build of `main` (compared by commit).
  Checks run automatically every 6 hours (can be switched off) and manually with a button; a dot on *Settings* and a banner
  show when an update is available.
* **One-button update** downloads `streamlink-studio_linux_<arch>.tar.gz`, **refuses to continue without a matching
  `checksums.txt` entry**, test-runs the new binary, keeps the old one as `streamlink-studio.prev`, swaps the file and
  restarts (active recordings are stopped and saved first; the page reloads when the new version is up).
  The restart needs systemd (`install.sh` sets `Restart=always` and gives the service user write access to the install
  directory). Older installs or other setups see the manual `install.sh --update` command instead – run it once to enable the button.
* `STUDIO_UPDATE_REPO` / `STUDIO_UPDATE_API` override the GitHub repository / API base (forks, tests).

## Run from source

```bash
make build && STUDIO_DATA=./data ./streamlink-studio     # http://localhost:8080
```

Requires Go ≥ 1.24, plus `streamlink` and `ffmpeg`/`ffprobe` on `PATH`. **No third-party Go modules.**

| Env var | Flag | Default |
|---|---|---|
| `STUDIO_ADDR` | `-addr` | `:8080` |
| `STUDIO_DATA` | `-data` | `./data` |
| `STUDIO_RECORDINGS` | `-rec` | `<data>/recordings` |
| `STUDIO_USER` / `STUDIO_PASS` | – | enables the login page; unset = **no password** |
| `STUDIO_CHANNEL` | – | `stable`/`beta`/`nightly`: installed channel (set by `install.sh`) |
| `STREAMLINK_BIN` / `FFMPEG_BIN` | – | from `PATH` (also settable in the UI) |

> The panel stores tokens and cookies. Set `STUDIO_USER`/`STUDIO_PASS` and put it behind a TLS reverse proxy.

## Sign-in

With `STUDIO_USER`/`STUDIO_PASS` set, the panel shows a login page (en/uk/ru, dark/light).

* **Remembered login:** "Remember me" (on by default) keeps you signed in for 30 days (sliding renewal); without it the
  session lasts until the browser is closed (max 12 h). Sessions are stored server-side in `<data>/sessions.json`
  (token hashes only, mode `0600`), so they **survive restarts and updates**. The cookie is `HttpOnly`, `SameSite=Lax`
  and `Secure` behind HTTPS (`X-Forwarded-Proto`).
* **Brute-force protection:** 5 wrong attempts lock the client IP for 5 minutes (behind a local reverse proxy the
  `X-Forwarded-For` / `X-Real-IP` header is used). Cross-origin state-changing requests are rejected.
* **Scripts:** HTTP Basic (`curl -u user:pass ...`) is still accepted by the API.
* To change the password edit `/etc/streamlink-studio.env` and `systemctl restart streamlink-studio`.

## Quality selection (frame-rate aware)

Streamlink names streams `720p` up to 30 fps and `720p60` above that, so a broadcast offers *either* one.
Studio asks streamlink which streams the channel really offers and resolves your setting against that list
when the recording starts:

| Setting | 60 fps stream | 30 fps stream |
|---|---|---|
| `720p` | `720p60` | `720p` |
| `720p60` | `720p60` | `720p` (same height) |
| `720p30` | `720p60` (only variant) | `720p` |
| `900p` (not offered) | nearest lower height | nearest lower height |
| `1080p,720p,best` | first height that exists | first height that exists |

* If both variants of a height exist, the higher fps wins unless you ask for `720p30`; `_alt` duplicates are never preferred.
* If the requested height is missing, the nearest **lower** one is used (then the nearest higher), so "720p" works as a cap.
* `best`, `worst` and plain names (`audio_only`, `source`, …) work as before; `best` is always appended as a safety net.
* **Test Stream** shows the offered streams and which one will be recorded; running recordings show the actual stream.

## Notes

* **Cookies:** streamlink has no `--http-cookies-file`; Studio converts the uploaded Netscape `cookies.txt`
  into `http-cookie=` entries (only cookies matching the channel's host).
* **Secrets stay off the command line:** tokens, cookies, passwords and proxy credentials are written to a
  private (`0600`) temporary `--config` file for each run and deleted afterwards, so they don't show up in `ps`.
* **Recording flow:** streamlink writes `.ts`; when the stream ends it is remuxed to the container from the
  file-name template (default `.mp4`) and the `.ts` is removed. Stopping uses SIGINT so the file is closed cleanly.
* **Fast Cut** cuts on keyframes (lossless, may include a little extra before IN); **Accurate Cut** re-encodes.

## Add a language

Copy `web/locales/en.json` to `web/locales/es.json`, translate it, and register it in
`web/languages.config.js`. Missing keys fall back to English.

## API (used by the UI)

`GET /api/state` · `GET /api/stats` · `GET|DELETE /api/notifications` · `POST /api/notifications/read` · `GET /api/gotify` · `POST /api/gotify/test` · `GET /icons/{file}` · `GET|PUT /api/settings` · `POST /api/channels` · `PUT|DELETE /api/channels/{id}` ·
`POST /api/channels/{id}/{record|stop|check}` · `POST /api/channels/test` · `PUT|DELETE /api/channels/{id}/cookies` ·
`GET /api/recordings` · `GET /api/recordings/{name}/{stream|download}` · `DELETE /api/recordings/{name}` ·
`POST /api/recordings/{name}/{convert|trim}` · `POST /api/recordings/{delete|save|merge}` · `GET /api/jobs` · `GET /healthz`

## Roadmap

Twitch token validation, Telegram/Discord and browser/desktop notifications, upload to Google Drive / S3, automatic cleanup when the disk fills up.
