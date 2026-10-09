// Package sloflix is the upstream side of sloflixfs: the sloflix.com API and its on-disk caches, DoodStream
// and CDN access with their rate limits, per-title metadata, and the background crawler that verifies titles.
package sloflix

import (
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Config sets up the upstream side (see Start).
type Config struct {
	CacheDir           string
	Refresh            time.Duration // how long listings are cached before the crawler refetches them
	Revalidate         time.Duration // how often each playable title is rechecked
	ProbeCache         int           // max probe heads waiting for Jellyfin's probe (0 = off)
	Rate               float64       // max sloflix API requests per second
	Concurrency        int           // max upstream lookups and fetches in flight; playback is exempt
	Username, Password string
	JellyfinURL        string // if set, scan the libraries under JellyfinPath when new titles are verified
	JellyfinKey        string
	JellyfinPath       string // the mount as Jellyfin sees it
}

// Start applies cfg, creates the cache directories and starts the API rate limiter. It doesn't log in.
func Start(cfg Config) error {
	cacheDir, refreshTTL, metaTTL, probeCache = cfg.CacheDir, cfg.Refresh, cfg.Revalidate, cfg.ProbeCache
	username, password = cfg.Username, cfg.Password
	jfURL, jfKey, jfPath = cfg.JellyfinURL, cfg.JellyfinKey, cfg.JellyfinPath
	slots = make(chan struct{}, cfg.Concurrency)
	go throttleLoop(time.Duration(float64(time.Second)/cfg.Rate), hiQ, loQ)
	// A previous run (just restarted) may have used DoodStream's limit: count the crawler's share as used,
	// so it waits a window while playback keeps its reserve.
	doodSent = slices.Repeat([]time.Time{time.Now()}, doodMax-doodReserve)
	for _, d := range []string{"json", "meta", "subs", "heads"} {
		if err := os.MkdirAll(filepath.Join(cacheDir, d), 0755); err != nil {
			return err
		}
	}
	return nil
}

// Login logs in (or loads the cached token) once, so bad credentials fail at startup instead of on every
// filesystem operation.
func Login() error {
	_, err := login(false)
	return err
}

// Crawl runs the crawler forever. limitMovies and limitShows, if > 0, limit it to the newest titles.
func Crawl(limitMovies, limitShows int) { crawler(limitMovies, limitShows) }
