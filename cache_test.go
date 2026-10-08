package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCachedShrinkGuard(t *testing.T) {
	cacheDir = t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "json"), 0755)
	refreshTTL = 0 // every call refetches, unless a refused refresh pinned the stale copy
	slots = make(chan struct{}, 1)
	n := 10
	get := func() int {
		t.Helper()
		v, err := cached("list", func() ([]item, error) { return make([]item, n), nil })
		if err != nil {
			t.Fatal(err)
		}
		return len(v)
	}
	expire := func() { delete(mem, "list") } // skip the failTTL pin so the next call refetches

	if got := get(); got != 10 {
		t.Fatalf("initial: %d", got)
	}
	n = 9 // 10% drop is within tolerance
	if got := get(); got != 9 {
		t.Fatalf("small drop refused: %d", got)
	}
	n = 3
	if got := get(); got != 9 {
		t.Fatalf("big drop accepted: %d", got)
	}
	expire()
	if got := get(); got != 9 {
		t.Fatalf("big drop accepted before %v: %d", shrinkAccept, got)
	}
	shrunk["list"] = time.Now().Add(-shrinkAccept)
	expire()
	if got := get(); got != 3 {
		t.Fatalf("persistent drop not accepted: %d", got)
	}
}
