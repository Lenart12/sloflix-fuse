# sloflixfs

A read-only FUSE filesystem that exposes the [sloflix.com](https://www.sloflix.com/) catalog as a Jellyfin-compatible media library. Video files are thin wrappers: their bytes are streamed on demand from the provider with HTTP range requests, so nothing but metadata is stored locally.

Docker image: [`lenart12/sloflixfs`](https://hub.docker.com/r/lenart12/sloflixfs) (linux/amd64, linux/arm64).

> Unofficial project, not affiliated with sloflix. It uses the site's private web API, which may change or block it at any time. You need your own sloflix account.

```
Movies/
  Runner (2026)/
    Runner (2026).mp4
    Runner (2026).sl.vtt          # Slovenian subtitles, when available
    movie.nfo                     # title, plot, genres, artwork from sloflix
Shows/
  Lanterns (2026)/
    tvshow.nfo
    Season 01/
      Lanterns (2026) S01E01.mp4
      Lanterns (2026) S01E01.sl.vtt
```

## Features

- **Streams on demand.** Opening a file fetches a fresh stream URL (from sloflix's direct link, or extracted from a DoodStream embed when there is none); reads are served from one HTTP range response per open file and only reconnect on seeks. Expired URLs and stalled connections are re-resolved automatically.
- **Jellyfin layout.** `Name (Year)` folders, `SxxEyy` episodes, `.sl.vtt` sidecar subtitles, and NFO files carrying sloflix's Slovenian title, plot, genres, poster and backdrop (images sloflix embeds inline become `poster.jpg`/`fanart.jpg` files). Titles TMDB can't match still get metadata.
- **Lists only what plays.** A background crawler checks each title (newest first) and the tree shows only titles it found playable. Library scans read only the cache, so they finish quickly and never wait on upstream. For each new title, the crawler also saves the start of the file that Jellyfin's probe reads, so probing it needs no stream lookup.
- **Gentle on upstream.** All API calls go through a rate limit (default 1/s) and a concurrency limit (default 3). Listings, file sizes and subtitles are cached on disk.
- **Tracks upstream changes.** Opening a file revalidates its size and subtitle, and the crawler re-checks every title about once a week. Changed titles get a new mtime so Jellyfin re-probes them; titles whose source disappears drop out of the listing after a day, so one wrong "gone" from upstream can't make Jellyfin delete them.
- **Protects your library.** A listing refresh that loses more than 10% of its entries is treated as an upstream glitch: the previous list keeps being served (and retried hourly) unless the drop persists for 24 hours, so a bad API response can't make Jellyfin delete titles and their watch history.
- **Playback first.** Lookups for playback jump the rate-limit queue and bypass the concurrency limit, and the crawler leaves part of DoodStream's limit for them, so crawling doesn't delay pressing play.

## Quick start (Docker Compose)

Runs the mount and Jellyfin together. Requires a Linux host with FUSE (`/dev/fuse`).

```sh
git clone https://github.com/Lenart12/sloflixfs.git
cd sloflixfs
cp .env.example .env    # fill in SLOFLIX_USER and SLOFLIX_PASS
docker compose pull
docker compose up -d
```

This runs the published `lenart12/sloflixfs` image. To build from source instead, use `docker compose up -d --build`. To update, run `docker compose pull && docker compose up -d`.

Jellyfin is then available at http://localhost:8096. Add two libraries:

| Library type | Folder |
|---|---|
| Movies | `/media/sloflix/library/Movies` |
| Shows | `/media/sloflix/library/Shows` |

In each library's settings, **disable** these, since they read whole files from upstream or don't work on FUSE:

- Trickplay image extraction
- Chapter image extraction
- Real-time monitoring

For a first try, set `SLOFLIX_ARGS=-limit-movies 20 -limit-shows 2` in `.env` to list only the newest titles.

### How the Docker setup works

- The `sloflix` container creates the FUSE mount itself (`/dev/fuse` and `SYS_ADMIN`; AppArmor unconfined on Ubuntu hosts). It appears on the host at `$SLOFLIX_DIR/library` (default `/mnt/sloflix/library`) through an `rshared` bind mount.
- Jellyfin binds the same directory with `rslave` and starts only after the mount passes its health check, so it never scans an empty folder.
- The parent directory is bound rather than the mount itself. A mount left dead by a crash therefore can't stop the containers from starting; `sloflix` detaches it on startup and Docker restarts the container.

## Running without Docker

Requires Go 1.25+ and FUSE 3 (`fusermount3`).

```sh
go build -o sloflixfs .
SLOFLIX_USER=... SLOFLIX_PASS=... ./sloflixfs -mount ~/sloflix
```

To let another user (such as a `jellyfin` service account or a container) read the mount, add `user_allow_other` to `/etc/fuse.conf` and pass `-allow-other`. Unmount with `fusermount3 -u ~/sloflix` or by stopping the process.

## Configuration

Credentials come from the `SLOFLIX_USER` and `SLOFLIX_PASS` environment variables. Flags:

| Flag | Default | Description |
|---|---|---|
| `-mount` | (required) | Mountpoint; created if missing. |
| `-cache` | `~/.cache/sloflixfs` | Cache directory (`/cache` in Docker). |
| `-refresh` | `12h` | How long catalog, season and episode listings are cached. |
| `-revalidate` | `168h` | How often each title's size and subtitle are re-checked in the background. |
| `-probe-cache` | `500` | Max new titles whose start is saved so Jellyfin's first probe needs no stream lookup (about 10–16 MB each, deleted once probed; 500 is up to about 8 GB). Beyond it, and with `0`, probes stream as usual. |
| `-rate` | `1` | Max API requests per second. |
| `-concurrency` | `3` | Max upstream lookups/fetches in flight (playback is exempt). |
| `-allow-other` | `false` | Allow other users to access the mount. |
| `-limit-movies`, `-limit-shows` | `0` (all) | List and crawl only the newest N titles; for testing. |

With Docker Compose, put extra flags in `SLOFLIX_ARGS` in `.env`.

## Caching and upstream requests

| What | Stored | Refreshed |
|---|---|---|
| Login token | disk | when missing or rejected |
| Catalog, season and episode listings | memory + disk | by the crawler after `-refresh` (listings always serve the cached copy); the old copy is kept if the refresh fails or shrinks by >10% (accepted after 24h) |
| File size, subtitle name, plot per title | disk | on open, and by the crawler every `-revalidate` (titles without a source: every `-refresh`) |
| Start of each new title's file (through the MP4 index, plus 7 MiB), up to `-probe-cache` titles | disk | deleted when the file is first closed (Jellyfin's probe), or after 3 days |
| Subtitle files | disk | in the background, revalidated by ETag |
| Stream URLs | memory | after 4h, or when the CDN rejects one |
| Video data | not cached | |

A title appears once the crawler has checked it: one API call, a CDN request for the file size and the start of the file, and, for titles with only a DoodStream embed link, a DoodStream lookup. With an empty cache the library fills in over a few days, newest titles first, because DoodStream lookups are limited to one per 26 seconds (see Limitations). Jellyfin's scheduled scans pick up whatever has appeared since the last one. Opening a file costs one API call (cached for 4 hours) and then streams; Jellyfin's first probe of a new title is served from the saved start instead. API, DoodStream and size requests are logged one per line, and each closed file logs a summary of its stream connections, so `docker compose logs -f sloflix` shows what the cache is doing.

## Limitations

- The CDN serves about 550 kB/s per connection, roughly 2–3× a typical bitrate here. That's fine for direct play, but slow for anything that reads whole files.
- Supported sources: sloflix's direct links (DoodStream CDN or presigned Cloudflare R2 URLs) and DoodStream embed/download links, which are resolved through a working DoodStream mirror because many of sloflix's embed links point at dead mirror domains. StreamP2P-only titles and titles whose DoodStream video was deleted are not listed; that was about 8% of the catalog when tested.
- DoodStream allows 15 lookups per IP per 5 minutes; after that it answers with a captcha (Cloudflare Turnstile) for about 5 minutes. Only lookups that get as far as requesting the video link count: deleted videos and player pages don't. All its mirror domains redirect to the same backend, so they share that limit, and so does every machine behind the same public IP. sloflixfs reserves 3 of the 15 for playback and spaces the crawler's lookups evenly over the rest: 5m10s (the 5-minute window plus margin) / 12 = one every 26 seconds. After a restart the crawler first waits one window. Opening a file (playback or a Jellyfin probe) can use all 15: when none is free it waits up to a minute for one, and the crawler holds back meanwhile; if none frees up within the minute, the open fails right away instead of hanging. This only matters for titles with just an embed link (about 30% of the catalog). If a captcha shows up anyway, the lookup fails as a temporary error (logged as `asks for a captcha`) and is retried later; sloflixfs doesn't try to get past it.
- Files under 5 MB are treated as broken uploads (sloflix has a few, e.g. a 64 KB film) and hidden like titles without a source; they're rechecked every `-refresh` in case they're re-uploaded.
- Titles that were never playable and whose CDN host is unreachable are hidden and retried hourly. Titles that were playable stay listed through such errors, and are only hidden once upstream has reported them gone for 24 hours.
- Sloflix only provides Slovenian and English titles. NFO files set the Slovenian title; the original-language title comes from TMDB when Jellyfin can match the item.
- Some of DoodStream's video servers still serve files under a `*.cloudatacdn.com` certificate that expired on 2026-08-01. sloflixfs accepts an expired certificate for that domain only, and only if its chain and hostname verify as of its expiry date; every other host gets normal verification. Each accepted host is logged once.
- Requests send a browser User-Agent to pass Cloudflare. Stricter bot protection on sloflix's side would stop the mount from working.

## Development

```sh
go vet ./... && go test ./...
```

Release (multi-platform image):

```sh
git tag vX.Y.Z && git push origin vX.Y.Z
docker buildx build --platform linux/amd64,linux/arm64 -t lenart12/sloflixfs:X.Y.Z -t lenart12/sloflixfs:latest --push .
```

`api.go` holds the sloflix client, rate limiting and caches; `fs.go` the filesystem tree and streaming; `main.go` flags and mounting.

## License

[MIT](LICENSE)
