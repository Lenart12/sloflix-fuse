package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The crawler checks new titles first (newest first), then rechecks oldest first, skipping recent failures;
// listings show only titles it verified.
func TestCrawlQueue(t *testing.T) {
	cacheDir = t.TempDir()
	for _, d := range []string{"json", "meta", "heads"} {
		os.MkdirAll(filepath.Join(cacheDir, d), 0755)
	}
	refreshTTL, metaTTL = time.Hour, 24*time.Hour
	mem, crawlFailed = map[string]memEntry{}, map[int]time.Time{}
	listing := func(key string, v any) {
		b, _ := json.Marshal(v)
		os.WriteFile(filepath.Join(cacheDir, "json", key+".json"), b, 0644)
	}
	metaAt := func(id int, size int64, age time.Duration) {
		writeMeta(id, meta{Size: size})
		os.Chtimes(metaFile(id), time.Now().Add(-age), time.Now().Add(-age))
	}
	listing("catalog-1", []item{
		{ID: 1, NameEn: "One", Created: "2026-01-02 00:00:00"}, // new
		{ID: 2, NameEn: "Two", Created: "2026-01-03 00:00:00"}, // new, newer
		{ID: 3, NameEn: "Three"},                               // no source, past refreshTTL
		{ID: 4, NameEn: "Four"},                                // playable, past metaTTL
		{ID: 5, NameEn: "Five"},                                // playable, fresh
		{ID: 6, NameEn: "Six"},                                 // new, but failed recently
	})
	listing("catalog-2", []item{{ID: 10, NameEn: "Show"}, {ID: 20, NameEn: "Dead show"}, {ID: 30, NameEn: "Unseen"}})
	listing(showKey(10), showInfo{Seasons: []int{1, 2}})
	listing(episodesKey(10, 1), []item{{ID: 11}, {ID: 12, Created: "2026-01-01 00:00:00"}})
	listing(episodesKey(10, 2), []item{{ID: 13}})
	listing(showKey(20), showInfo{Seasons: []int{1}})
	listing(episodesKey(20, 1), []item{{ID: 21}})
	metaAt(3, 0, 2*time.Hour)
	metaAt(4, 100<<20, 48*time.Hour)
	metaAt(5, 100<<20, 0)
	metaAt(11, 100<<20, 0)
	metaAt(13, 0, 0)
	metaAt(21, 0, 3*time.Hour)
	crawlFailed[6] = time.Now()
	listing(showKey(30), showInfo{Seasons: []int{1}}) // its episodes were never fetched; the crawler fetches them
	slots = make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() { // the crawler fetches show 30's episodes
		for {
			select {
			case loQ <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()
	apiBase, token = "http://127.0.0.1:1", "test" // the fetch fails fast; the crawler goes on

	if got, want := crawlQueue(0, 0), []int{2, 1, 12, 21, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("queue %v, want %v", got, want)
	}
	if got, want := crawlQueue(1, 0), []int{1, 12, 21}; !slices.Equal(got, want) {
		t.Fatalf("queue with -limit-movies 1: %v, want %v", got, want)
	}

	names := func(list func() ([]child, error)) []string {
		t.Helper()
		children, err := list()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range children {
			if c.listed() {
				out = append(out, c.name)
			}
		}
		return out
	}
	checked := 0
	if _, err := catalogDir(1, 0, func(int) bool { checked++; return true }, movieDir)(); err != nil || checked != 0 {
		t.Fatalf("building the listing checked %d titles (err %v); Lookup must check only the one it finds", checked, err)
	}
	if got, want := names(catalogDir(1, 0, movieVisible, movieDir)), []string{"Four", "Five"}; !slices.Equal(got, want) {
		t.Fatalf("movies %v, want %v", got, want)
	}
	if got, want := names(catalogDir(2, 0, showVisible, showDir)), []string{"Show"}; !slices.Equal(got, want) {
		t.Fatalf("shows %v, want %v", got, want)
	}
	if got := names(showDir(item{ID: 10}, "Show")); !slices.Contains(got, "Season 01") || slices.Contains(got, "Season 02") {
		t.Fatalf("show dir %v: want Season 01 only (season 2 has no playable episode)", got)
	}
}
