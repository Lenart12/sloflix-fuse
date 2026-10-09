package fusefs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Lenart12/sloflixfs/internal/sloflix"
)

// Listings show only titles the crawler verified as playable, from cached listings alone.
func TestListing(t *testing.T) {
	dir := t.TempDir()
	if err := sloflix.Start(sloflix.Config{CacheDir: dir, Rate: 1, Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	write := func(path string, v any) {
		b, _ := json.Marshal(v)
		os.WriteFile(filepath.Join(dir, path), b, 0644)
	}
	write("json/catalog-1.json", []sloflix.Item{{ID: 1, NameEn: "One"}, {ID: 3, NameEn: "Three"}, {ID: 4, NameEn: "Four"}, {ID: 5, NameEn: "Five"}})
	write("json/catalog-2.json", []sloflix.Item{{ID: 10, NameEn: "Show"}, {ID: 20, NameEn: "Dead show"}, {ID: 30, NameEn: "Unseen"}})
	write("json/show-10.json", sloflix.ShowInfo{Seasons: []int{1, 2}})
	write("json/episodes-10-1.json", []sloflix.Item{{ID: 11}, {ID: 12}})
	write("json/episodes-10-2.json", []sloflix.Item{{ID: 13}})
	write("json/show-20.json", sloflix.ShowInfo{Seasons: []int{1}})
	write("json/episodes-20-1.json", []sloflix.Item{{ID: 21}})
	write("json/show-30.json", sloflix.ShowInfo{Seasons: []int{1}}) // episodes never fetched
	for id, size := range map[int]int64{3: 0, 4: 100 << 20, 5: 100 << 20, 11: 100 << 20, 13: 0, 21: 0} {
		write(fmt.Sprintf("meta/%d.json", id), sloflix.Meta{Size: size})
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
	if got := names(showDir(sloflix.Item{ID: 10}, "Show")); !slices.Contains(got, "Season 01") || slices.Contains(got, "Season 02") {
		t.Fatalf("show dir %v: want Season 01 only (season 2 has no playable episode)", got)
	}
}
