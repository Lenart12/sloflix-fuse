// Command sloflixfs mounts the sloflix.com catalog as a read-only FUSE filesystem in Jellyfin's layout.
package main

import (
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/Lenart12/sloflixfs/internal/fusefs"
	"github.com/Lenart12/sloflixfs/internal/sloflix"
)

func main() {
	userCache, _ := os.UserCacheDir()
	var cfg sloflix.Config
	mount := flag.String("mount", "", "mountpoint (required)")
	flag.StringVar(&cfg.CacheDir, "cache", filepath.Join(userCache, "sloflixfs"), "cache directory")
	flag.DurationVar(&cfg.Refresh, "refresh", 12*time.Hour, "how long catalog listings are cached")
	flag.DurationVar(&cfg.Revalidate, "revalidate", 7*24*time.Hour, "how often each title's size and subtitle are revalidated in the background")
	flag.IntVar(&cfg.ProbeCache, "probe-cache", 500, "max new titles whose start is saved for Jellyfin's first probe, about 10-16 MB each until probed (0 = off)")
	flag.Float64Var(&cfg.Rate, "rate", 1, "max API requests per second")
	flag.IntVar(&cfg.Concurrency, "concurrency", 3, "max upstream lookups/fetches in flight (playback is exempt)")
	limitMovies := flag.Int("limit-movies", 0, "list only the newest N movies (0 = all), for testing")
	limitShows := flag.Int("limit-shows", 0, "list only the newest N shows (0 = all), for testing")
	allowOther := flag.Bool("allow-other", false, "let other users (e.g. Jellyfin in Docker) access the mount")
	flag.Parse()
	cfg.Username, cfg.Password = os.Getenv("SLOFLIX_USER"), os.Getenv("SLOFLIX_PASS")
	if *mount == "" || cfg.Username == "" || cfg.Password == "" {
		log.Fatal("usage: SLOFLIX_USER=.. SLOFLIX_PASS=.. sloflixfs -mount DIR")
	}
	if cfg.Rate <= 0 || cfg.Concurrency <= 0 {
		log.Fatal("-rate and -concurrency must be positive")
	}
	if err := sloflix.Start(cfg); err != nil {
		log.Fatal(err)
	}
	if err := sloflix.Login(); err != nil {
		log.Fatal(err)
	}

	// Clear a dead mount left by a previous crash, or mounting fails. Needs root (e.g. in Docker).
	if _, err := os.Stat(*mount); errors.Is(err, syscall.ENOTCONN) {
		syscall.Unmount(*mount, syscall.MNT_DETACH)
	}
	if err := os.MkdirAll(*mount, 0755); err != nil {
		log.Fatal(err)
	}
	timeout := time.Hour
	server, err := fs.Mount(*mount, fusefs.NewRoot(*limitMovies, *limitShows), &fs.Options{
		EntryTimeout: &timeout,
		AttrTimeout:  &timeout,
		MountOptions: fuse.MountOptions{
			AllowOther:         *allowOther,
			FsName:             "sloflix",
			Name:               "sloflixfs",
			SyncRead:           true,
			DisableReadDirPlus: true,
			MaxReadAhead:       1 << 20,
			Options:            []string{"ro"},
			DirectMount:        true,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mounted on %s", *mount)
	go sloflix.Crawl(*limitMovies, *limitShows)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		// Lazy unmount first: unlike a plain one it also propagates to busy copies of the mount (the host's and
		// Jellyfin's in Docker), so stopping while files are open doesn't leave a dead mount behind. It needs
		// root; otherwise fall back to fusermount.
		if err := syscall.Unmount(*mount, syscall.MNT_DETACH); err != nil {
			if err := server.Unmount(); err != nil {
				log.Printf("unmount: %v", err)
			}
			return
		}
		// Files still open keep a detached mount's connection (and server.Wait) alive; don't wait for them.
		time.AfterFunc(2*time.Second, func() { os.Exit(0) })
	}()
	server.Wait()
}
