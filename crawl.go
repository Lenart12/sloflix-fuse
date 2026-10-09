package main

import (
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// due reports whether the crawler should check id now, and in which order: bucket 0 is never checked,
// 1 a title without a source or in its goneGrace period (rechecked after refreshTTL), 2 a playable one
// (rechecked after metaTTL). written is when its meta was last written.
func due(id int) (bucket int, written time.Time, ok bool) {
	lookupMu.Lock()
	recent := time.Since(crawlFailed[id]) < failTTL
	lookupMu.Unlock()
	if recent {
		return 0, time.Time{}, false
	}
	m, written, known := readMeta(id)
	switch {
	case !known:
		return 0, written, true
	case m.Size == 0 || !m.Gone.IsZero():
		return 1, written, time.Since(written) >= refreshTTL
	default:
		return 2, written, time.Since(written) >= metaTTL
	}
}

// crawler checks titles in the background, so listings only show playable titles and never wait on
// upstream: new titles first (newest first), then rechecks (see due), oldest first. This also catches
// changed or removed sources and subtitles added or edited upstream for titles nobody plays. The rate
// limits pace it; DoodStream's leaves a reserve for playback (doodTake). Workers beyond -concurrency (which
// still caps requests in flight) can wait for DoodStream without holding up titles that don't need it.
func crawler(limitMovies, limitShows int) {
	for {
		queue := crawlQueue(limitMovies, limitShows)
		// Rebuild at most every 10 minutes, so newly listed titles don't wait behind a long recheck queue.
		next := time.Now().Add(10 * time.Minute)
		ids := make(chan int)
		var wg sync.WaitGroup
		for range cap(slots) + doodMax {
			wg.Go(func() {
				for id := range ids {
					crawl(id)
				}
			})
		}
		for _, id := range queue {
			if time.Now().After(next) {
				break
			}
			ids <- id
		}
		close(ids)
		wg.Wait()
		time.Sleep(time.Until(next))
	}
}

// crawlQueue walks the catalog (the only place its listings are refreshed; readdir serves them cached) and returns the titles
// due for a check, in order. It also removes probe caches Jellyfin never read.
func crawlQueue(limitMovies, limitShows int) []int {
	type task struct {
		id, bucket int
		at         time.Time
	}
	var tasks []task
	add := func(id int, created time.Time) {
		if bucket, written, ok := due(id); ok {
			if bucket == 0 {
				written = created
			}
			tasks = append(tasks, task{id, bucket, written})
		}
	}
	movies, err := catalog(1)
	if err != nil {
		log.Printf("crawl: %v", err)
	}
	for _, it := range firstN(movies, limitMovies) {
		add(it.ID, parseTime(it.Created))
	}
	shows, err := catalog(2)
	if err != nil {
		log.Printf("crawl: %v", err)
	}
	for _, show := range firstN(shows, limitShows) {
		si, err := showMeta(show.ID)
		if err != nil {
			log.Printf("crawl: show %d: %v", show.ID, err)
			continue
		}
		for _, s := range si.Seasons {
			eps, err := episodes(show.ID, s)
			if err != nil {
				log.Printf("crawl: show %d season %d: %v", show.ID, s, err)
			}
			for _, ep := range eps {
				add(ep.ID, parseTime(ep.Created))
			}
		}
	}
	slices.SortFunc(tasks, func(a, b task) int {
		if a.bucket != b.bucket {
			return a.bucket - b.bucket
		}
		if a.bucket == 0 {
			return b.at.Compare(a.at) // newest first
		}
		return a.at.Compare(b.at) // least recently checked first
	})
	ids, fresh := make([]int, len(tasks)), 0
	for i, t := range tasks {
		ids[i] = t.id
		if t.bucket == 0 {
			fresh++
		}
	}
	heads, _ := os.ReadDir(filepath.Join(cacheDir, "heads"))
	for _, e := range heads {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > headTTL {
			os.Remove(filepath.Join(cacheDir, "heads", e.Name()))
		}
	}
	if len(ids) > 0 {
		log.Printf("crawl: %d new, %d to recheck; %d of %d probe cache slots used", fresh, len(ids)-fresh, headCount(), probeCache)
	}
	return ids
}

// crawl checks one title, unless playback did meanwhile, and revalidates its cached subtitle.
func crawl(id int) {
	if _, _, ok := due(id); !ok {
		return
	}
	_, m, err := resolve(id, false)
	if err != nil {
		lookupMu.Lock()
		crawlFailed[id] = time.Now() // transient errors aren't persisted; don't retry for failTTL
		lookupMu.Unlock()
		log.Printf("crawl %d: %v", id, err)
		return
	}
	if _, err := os.Stat(subFile(m.Sub)); m.Sub == "" || err != nil {
		return
	}
	if changed, err := fetchSubtitle(m.Sub); err != nil {
		log.Printf("crawl %d: %v", id, err)
	} else if changed {
		log.Printf("media %d: subtitle %s changed", id, m.Sub)
		m.Changed = time.Now()
		writeMeta(id, m)
	}
}
