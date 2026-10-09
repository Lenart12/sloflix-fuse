package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type urlEntry struct {
	url  string
	size int64
	at   time.Time
}

// meta is what we persist per playable item: size for getattr, subtitle for listing the .vtt.
// Size 0 means no playable source was found. Changed is when a re-resolve last saw Size or Sub differ.
type meta struct {
	Size    int64     `json:"size"`
	Sub     string    `json:"sub"`
	Plot    string    `json:"plot"`
	Changed time.Time `json:"changed"`
	Gone    time.Time `json:"gone,omitzero"` // when upstream first reported a listed title gone (see goneGrace)
	// DeadCode is a DoodStream ID reported deleted. IDs aren't reused, so while sloflix keeps returning it,
	// rechecks of a hidden title skip DoodStream.
	DeadCode string `json:"dead_code,omitempty"`
}

// resolve fetches a fresh stream URL for id, records its size and subtitle in meta, and memoizes the URL.
func resolve(id int, hi bool) (string, meta, error) {
	release := acquire(hi)
	defer func() { release() }()
	var d struct {
		Plot    string `json:"media_description"`
		Sources []struct {
			Source string  `json:"media_source"`
			Name   string  `json:"media_source_name"`
			Sub    *string `json:"subtitle_location"`
		} `json:"media_sources"`
	}
	if err := api(fmt.Sprintf("/media/single/%d?dont_count_view=true", id), &d, hi); err != nil {
		return "", meta{}, err
	}
	m := meta{Plot: d.Plot}
	// Prefer sloflix's own direct link; otherwise extract one from a DoodStream embed.
	var stream, doodCode string
	var doodSub *string
	for _, s := range d.Sources {
		u, err := url.Parse(s.Source)
		if err != nil {
			continue
		}
		if u.Host == "player.sloflix.com" {
			stream = strings.TrimSpace(u.Query().Get("source")) // some carry a trailing newline
			if s.Sub != nil {
				m.Sub = *s.Sub
			}
			break
		}
		// Embed (/e/) or download-page (/d/) links; the mirror serves either code under /e/.
		if doodCode == "" && strings.Contains(strings.ToLower(s.Name), "doodstream") && (strings.HasPrefix(u.Path, "/e/") || strings.HasPrefix(u.Path, "/d/")) {
			doodCode, doodSub = path.Base(u.Path), s.Sub
		}
	}
	// missing is set when the source exists but its video is gone; that's persisted as no source, while
	// other errors (dead hosts, timeouts) are transient.
	prev, _, hasPrev := readMeta(id)
	var missing error
	if stream == "" && doodCode != "" {
		var err error
		if doodCode == prev.DeadCode && prev.Size == 0 { // a title in goneGrace (Size > 0) is verified again
			err = errVideoGone
		} else {
			stream, err = doodURL(doodCode, func() error {
				// Give up the concurrency slot while queued for DoodStream, so lookups that don't need it go on.
				release()
				defer func() { release = acquire(hi) }()
				return doodTake(hi)
			})
		}
		if err != nil {
			if !errors.Is(err, errVideoGone) {
				return "", meta{}, err
			}
			missing = fmt.Errorf("DoodStream %s: %w", doodCode, err)
			m.DeadCode = doodCode
		}
		if doodSub != nil {
			m.Sub = *doodSub
		}
	}
	if stream != "" && hi && prev.Size > 0 {
		// Playback skips the size probe: the stream checks the size on every response (stream.open).
		m.Size = prev.Size
	} else if stream != "" {
		start := time.Now()
		headID := 0
		if !hi && prev.Size == 0 && headCount() < probeCache { // a new title: save its start for Jellyfin's probe
			headID = id
		}
		size, err := probeSize(stream, headID)
		if err == nil {
			u, _ := url.Parse(stream)
			log.Printf("cdn: probe %d %s %v", id, u.Host, time.Since(start).Round(time.Millisecond))
		}
		if errors.Is(err, errFileGone) {
			stream, missing = "", err
		} else if err != nil {
			return "", meta{}, err
		} else if size < minVideoSize {
			stream, missing = "", fmt.Errorf("file is only %d bytes, likely a broken upload", size)
			size = 0 // no source: rechecked after refreshTTL, in case it's re-uploaded
			os.Remove(headFile(id))
		}
		m.Size = size
	}
	var noSource error
	if stream == "" && missing != nil {
		noSource = fmt.Errorf("no direct source: %w", missing)
	} else if stream == "" {
		var got []string
		for _, s := range d.Sources {
			u, _ := url.Parse(s.Source)
			got = append(got, fmt.Sprintf("%s@%s", s.Name, u.Host))
		}
		noSource = fmt.Errorf("no direct source (got %d: %v)", len(d.Sources), got)
	}
	m.Changed = prev.Changed
	if noSource != nil && prev.Size > 0 {
		gone := prev.Gone
		if gone.IsZero() {
			gone = time.Now()
		}
		if time.Since(gone) < goneGrace {
			m.Size, m.Gone = prev.Size, gone
			writeMeta(id, m)
			return "", m, fmt.Errorf("%w; still listed until %s", noSource, gone.Add(goneGrace).Format(time.DateTime))
		}
	}
	if hasPrev && (prev.Size != m.Size || prev.Sub != m.Sub) {
		log.Printf("media %d changed: size %d -> %d, sub %q -> %q", id, prev.Size, m.Size, prev.Sub, m.Sub)
		m.Changed = time.Now()
		if prev.Size > 0 && prev.Size != m.Size {
			os.Remove(headFile(id)) // it's from the old file
		}
	}
	writeMeta(id, m)
	if noSource != nil {
		return "", m, noSource
	}
	urlMu.Lock()
	urls[id] = urlEntry{stream, m.Size, time.Now()}
	urlMu.Unlock()
	return stream, m, nil
}

// info returns a title's persisted meta. Titles the crawler hasn't verified, or found without a source, are
// an error. A playable title stays listed through transient errors and upstream reporting it gone within
// goneGrace (resolve keeps its size), so Jellyfin doesn't drop it and its watch history over a glitch.
func info(id int) (meta, error) {
	m, _, _ := readMeta(id)
	if m.Size < minVideoSize {
		return m, errNoSource
	}
	return m, nil
}

func metaFile(id int) string {
	return filepath.Join(cacheDir, "meta", fmt.Sprint(id)+".json")
}

func writeMeta(id int, m meta) {
	b, _ := json.Marshal(m)
	writeFile(metaFile(id), b)
}

func readMeta(id int) (meta, time.Time, bool) {
	var m meta
	st, err := os.Stat(metaFile(id))
	if err != nil {
		return m, time.Time{}, false
	}
	b, _ := os.ReadFile(metaFile(id))
	return m, st.ModTime(), json.Unmarshal(b, &m) == nil
}

// setSize records a size seen by a stream that differs from the cached one: the upstream file was replaced.
func setSize(id int, size int64) {
	m, _, _ := readMeta(id)
	log.Printf("media %d changed: size %d -> %d", id, m.Size, size)
	m.Size, m.Changed = size, time.Now()
	writeMeta(id, m)
	os.Remove(headFile(id)) // it's from the old file
	urlMu.Lock()
	if e, ok := urls[id]; ok {
		e.size = size
		urls[id] = e
	}
	urlMu.Unlock()
}

// streamURL returns a memoized stream URL and its size, or fresh ones if force is set or it is older than urlTTL.
// Resolving also refreshes the item's persisted meta, so opening a file revalidates its size and subtitle.
func streamURL(id int, force bool) (string, int64, error) {
	urlMu.Lock()
	e, ok := urls[id]
	urlMu.Unlock()
	if ok && !force && time.Since(e.at) < urlTTL {
		return e.url, e.size, nil
	}
	lookupMu.Lock()
	recent := !force && time.Since(failed[id]) < playFailTTL
	lookupMu.Unlock()
	if recent {
		return "", 0, errFailedRecently
	}
	u, m, err := resolve(id, true)
	if err != nil {
		lookupMu.Lock()
		failed[id] = time.Now()
		lookupMu.Unlock()
	}
	return u, m.Size, err
}
