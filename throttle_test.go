package main

import (
	"os"
	"slices"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	doodWindow = time.Microsecond // tests resolving DoodStream embeds don't wait for the window
	os.Exit(m.Run())
}

// Slots are never handed out closer than the interval, not even right after an idle period.
func TestThrottleSpacing(t *testing.T) {
	hi, lo := make(chan struct{}), make(chan struct{})
	go throttleLoop(50*time.Millisecond, hi, lo)
	time.Sleep(120 * time.Millisecond) // idle
	prev := time.Now()
	<-lo
	for i := range 3 {
		take(i == 1, hi, lo)
		if gap := time.Since(prev); gap < 45*time.Millisecond {
			t.Fatalf("slot %d only %v after the previous one", i+2, gap)
		}
		prev = time.Now()
	}
}

func TestThrottlePrefersHi(t *testing.T) {
	hi, lo := make(chan struct{}), make(chan struct{}) // not hiQ/loQ: a loop left running would hand out ticks to later tests
	go throttleLoop(20*time.Millisecond, hi, lo)
	done := make(chan string, 6)
	for range 5 {
		go func() { take(false, hi, lo); done <- "lo" }()
	}
	time.Sleep(5 * time.Millisecond)
	go func() { take(true, hi, lo); done <- "hi" }()
	for i := range 6 {
		if <-done == "hi" {
			if i > 1 {
				t.Fatalf("hi served at position %d behind queued lo waiters", i)
			}
			return
		}
	}
}

// The crawler stops at doodMax-doodReserve lookups per window; playback can still use the rest, and fails
// at once instead of waiting when the window is full.
func TestDoodWindow(t *testing.T) {
	defer func(w time.Duration) { doodWindow, doodSent, doodLastLo = w, nil, time.Time{} }(doodWindow)
	doodWindow, doodLastLo = 500*time.Millisecond, time.Time{}
	start := time.Now()
	doodSent = slices.Repeat([]time.Time{start}, doodMax-doodReserve) // the crawler's share, used
	crawled := make(chan struct{})
	go func() { doodTake(false); close(crawled) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-crawled:
		t.Fatal("crawler lookup exceeded its share")
	default:
	}
	for range doodReserve {
		if err := doodTake(true); err != nil {
			t.Fatalf("playback lookup within the reserve: %v", err)
		}
	}
	if err := doodTake(true); err == nil {
		t.Fatalf("lookup %d in a full window should fail", doodMax+1)
	}
	<-crawled // once the oldest lookups leave the window
	if waited := time.Since(start); waited < 450*time.Millisecond {
		t.Fatalf("crawler lookup %d only waited %v", doodMax-doodReserve+1, waited)
	}
}

// The crawler's lookups are spaced doodPace apart; playback isn't paced.
func TestDoodPace(t *testing.T) {
	defer func(w time.Duration) { doodWindow, doodSent, doodLastLo = w, nil, time.Time{} }(doodWindow)
	doodWindow, doodSent, doodLastLo = 1200*time.Millisecond, nil, time.Time{} // pace 100ms
	start := time.Now()
	doodTake(false)
	doodTake(true)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("first crawler lookup or playback waited")
	}
	doodTake(false)
	if waited := time.Since(start); waited < 90*time.Millisecond {
		t.Fatalf("second crawler lookup after %v, want the %v pace", waited, doodPace())
	}
}
