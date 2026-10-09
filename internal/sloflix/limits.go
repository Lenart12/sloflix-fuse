package sloflix

import (
	"fmt"
	"sync"
	"time"
)

// DoodStream lookups have their own limit: playmogo.com (where every mirror redirects) allows 15 pass_md5
// requests per IP per 5 minutes, then answers with RELOAD and a captcha for ~5 minutes (measured 2026-10-09).
// The crawler leaves doodReserve of them for playback.
const (
	doodMax     = 15
	doodReserve = 3
)

var (
	doodWindow = 5*time.Minute + 10*time.Second // the measured 5 minutes plus margin; a var for tests
	doodMu     sync.Mutex
	doodSent   []time.Time // DoodStream lookups within doodWindow, oldest first; guarded by doodMu
	doodLastLo time.Time   // the crawler's last lookup; guarded by doodMu
	// doodHiWaiting counts opens waiting for a slot; guarded by doodMu. doodHiWait bounds that wait: long
	// enough for a scan's probes to queue up, short enough that an open doesn't hang. A var for tests.
	doodHiWaiting int
	doodHiWait    = time.Minute
)

// doodPace spaces the crawler's lookups evenly over the window, at its share of it: 5m10s / (15-3) = 25.8s.
// Bursts of 12 would be just as much within the limit, but leave the crawler idle (and the log silent) for
// minutes, and would be the first thing to break if DoodStream ever penalised bursts.
func doodPace() time.Duration { return doodWindow / (doodMax - doodReserve) }

// doodTake waits until a DoodStream lookup fits the window, then records it. A sliding window of 15 also
// stays within a fixed window or token bucket of that size, whichever DoodStream uses. Playback (any open,
// including Jellyfin's probes) waits at most doodHiWait, and fails at once if no slot frees up by then;
// while it waits, the crawler holds back, so opens get the whole window.
func doodTake(hi bool) error {
	limit := doodMax
	if !hi {
		limit -= doodReserve
	}
	deadline, waiting := time.Now().Add(doodHiWait), false
	defer func() {
		if waiting {
			doodMu.Lock()
			doodHiWaiting--
			doodMu.Unlock()
		}
	}()
	for {
		doodMu.Lock()
		now := time.Now()
		for len(doodSent) > 0 && now.Sub(doodSent[0]) >= doodWindow {
			doodSent = doodSent[1:]
		}
		var wait time.Duration
		if len(doodSent) >= limit {
			wait = doodSent[len(doodSent)-limit].Add(doodWindow).Sub(now)
		}
		if !hi {
			wait = max(wait, doodLastLo.Add(doodPace()).Sub(now))
			if doodHiWaiting > 0 {
				wait = max(wait, time.Second) // an open is waiting: let it go first
			}
		}
		if wait <= 0 {
			doodSent = append(doodSent, now)
			if !hi {
				doodLastLo = now
			}
			doodMu.Unlock()
			return nil
		}
		if hi && now.Add(wait).After(deadline) {
			doodMu.Unlock()
			return fmt.Errorf("DoodStream lookup limit reached, free again in %v", wait.Round(time.Second))
		}
		if hi && !waiting {
			doodHiWaiting++
			waiting = true
		}
		doodMu.Unlock()
		time.Sleep(wait)
	}
}

// throttleLoop hands out one request slot at a time, at least every apart, preferring waiters on hi
// (playback) over lo. The first slot after an idle period goes out at once, but never two back to back.
func throttleLoop(every time.Duration, hi, lo chan struct{}) {
	for {
		select {
		case hi <- struct{}{}:
		default:
			select {
			case hi <- struct{}{}:
			case lo <- struct{}{}:
			}
		}
		time.Sleep(every)
	}
}

// acquire takes a concurrency slot for a lookup or fetch and returns its release. Take it before waiting for
// the rate limit so ticks aren't spent on requests that can't start. Playback (hi) bypasses the limit, so
// slots held by stuck background requests can't stall it; it still waits for the rate limit.
func acquire(hi bool) func() {
	if hi {
		return func() {}
	}
	slots <- struct{}{}
	return func() { <-slots }
}

// wait takes a sloflix API slot.
func wait(hi bool) { take(hi, hiQ, loQ) }

func take(hi bool, hq, lq chan struct{}) {
	if hi {
		<-hq
	} else {
		<-lq
	}
}
