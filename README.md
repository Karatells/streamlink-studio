# Streamlink Studio

A lightweight web panel that monitors channels, records live streams with
[`streamlink`](https://streamlink.github.io/), post-processes them with `ffmpeg`
and lets you play, convert and trim recordings in the browser.

* **Auto-monitoring & recording** – a background poller checks every channel and starts recording when it goes live.
* **Auth & plugins** – Twitch OAuth token (ad-free / sub-only), `cookies.txt` (YouTube members-only, 18+), HTTP proxy & headers (Kick), plugin e-mail/password. Global credentials with per-channel overrides.
* **Recordings library** – search/filter, in-browser player (HTTP range seeking), download, delete.
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
| `STUDIO_USER` / `STUDIO_PASS` | – | unset = **no password** |
| `STREAMLINK_BIN` / `FFMPEG_BIN` | – | from `PATH` (also settable in the UI) |

> The panel stores tokens and cookies. Set `STUDIO_USER`/`STUDIO_PASS` or put it behind a TLS reverse proxy.

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

`GET /api/state` · `GET|PUT /api/settings` · `POST /api/channels` · `PUT|DELETE /api/channels/{id}` ·
`POST /api/channels/{id}/{record|stop|check}` · `POST /api/channels/test` · `PUT|DELETE /api/channels/{id}/cookies` ·
`GET /api/recordings` · `GET /api/recordings/{name}/{stream|download}` · `DELETE /api/recordings/{name}` ·
`POST /api/recordings/{name}/{convert|trim}` · `GET /api/jobs` · `GET /healthz`

## Roadmap

GitHub-API update check with a one-click update button, Twitch token validation, Telegram/Discord
notifications, upload to Google Drive / S3, automatic cleanup when the disk fills up.
