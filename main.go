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
)

func main() {
	userCache, _ := os.UserCacheDir()
	mount := flag.String("mount", "", "mountpoint (required)")
	flag.StringVar(&cacheDir, "cache", filepath.Join(userCache, "sloflix-fuse"), "cache directory")
	flag.DurationVar(&refreshTTL, "refresh", 12*time.Hour, "how long catalog listings are cached")
	flag.DurationVar(&metaTTL, "revalidate", 7*24*time.Hour, "how often each title's size and subtitle are revalidated in the background")
	rate := flag.Float64("rate", 1, "max API requests per second")
	concurrency := flag.Int("concurrency", 3, "max upstream lookups/fetches in flight (playback is exempt)")
	limitMovies := flag.Int("limit-movies", 0, "list only the newest N movies (0 = all), for testing")
	limitShows := flag.Int("limit-shows", 0, "list only the newest N shows (0 = all), for testing")
	allowOther := flag.Bool("allow-other", false, "let other users (e.g. Jellyfin in Docker) access the mount")
	flag.Parse()
	username, password = os.Getenv("SLOFLIX_USER"), os.Getenv("SLOFLIX_PASS")
	if *mount == "" || username == "" || password == "" {
		log.Fatal("usage: SLOFLIX_USER=.. SLOFLIX_PASS=.. sloflix-fuse -mount DIR")
	}
	if *rate <= 0 || *concurrency <= 0 {
		log.Fatal("-rate and -concurrency must be positive")
	}
	slots = make(chan struct{}, *concurrency)
	go throttleLoop(time.Duration(float64(time.Second) / *rate))
	for _, d := range []string{"json", "meta", "subs"} {
		if err := os.MkdirAll(filepath.Join(cacheDir, d), 0755); err != nil {
			log.Fatal(err)
		}
	}
	// Fail fast on bad credentials instead of retrying the login on every filesystem operation.
	if _, err := login(false); err != nil {
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
	server, err := fs.Mount(*mount, newRoot(*limitMovies, *limitShows), &fs.Options{
		EntryTimeout: &timeout,
		AttrTimeout:  &timeout,
		MountOptions: fuse.MountOptions{
			AllowOther:         *allowOther,
			FsName:             "sloflix",
			Name:               "sloflix",
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
	go refresher()
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
