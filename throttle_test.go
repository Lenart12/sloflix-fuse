package main

import (
	"testing"
	"time"
)

func TestThrottlePrefersHi(t *testing.T) {
	go throttleLoop(20 * time.Millisecond)
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
