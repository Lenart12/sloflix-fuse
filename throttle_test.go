package main

import (
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	go throttleLoop(time.Microsecond, doodHiQ, doodLoQ) // tests resolving DoodStream embeds don't wait 21s
	os.Exit(m.Run())
}

// Slots are never handed out closer than the interval, not even right after an idle period: a burst would
// break DoodStream's 15-per-5-minutes quota.
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
	go throttleLoop(20*time.Millisecond, hiQ, loQ)
	done := make(chan string, 6)
	for range 5 {
		go func() { wait(false); done <- "lo" }()
	}
	time.Sleep(5 * time.Millisecond)
	go func() { wait(true); done <- "hi" }()
	for i := range 6 {
		if <-done == "hi" {
			if i > 1 {
				t.Fatalf("hi served at position %d behind queued lo waiters", i)
			}
			return
		}
	}
}
