package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var apiBase = "https://api.sloflix.com/v1" // a var for tests

const (
	subBase   = "https://sloflix.com/subtitles/"
	userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"
	referer   = "https://player.sloflix.com/"
	// Stream links stay valid for at least 4.6h (measured: still valid at 4.6h, "error_expired" at 18h);
	// a rejected link is re-resolved anyway.
	urlTTL  = 4 * time.Hour
	failTTL = time.Hour
	// A listing that shrinks by more than 10% is treated as an upstream glitch, so Jellyfin doesn't delete
	// the missing titles. Only a shrink that persists this long is accepted.
	shrinkAccept = 24 * time.Hour
	// A title that used to play stays listed this long after upstream first says it's gone, so one wrong
	// "gone" (an anti-bot page, a CDN hiccup) can't make Jellyfin drop it and its watch history.
	goneGrace = 24 * time.Hour
	// Playback failures are remembered this long, so a probe retrying a dead title doesn't refetch each time.
	playFailTTL = 5 * time.Minute
)

// Outcomes that were logged when first determined; callers don't log them again on every listing.
var (
	errNoSource       = errors.New("no direct source")
	errFailedRecently = errors.New("lookup failed recently")
)

func quiet(err error) bool { return errors.Is(err, errNoSource) || errors.Is(err, errFailedRecently) }

var (
	cacheDir   string
	refreshTTL time.Duration
	metaTTL    time.Duration
	probeCache int // -probe-cache: max saved starts of new titles awaiting Jellyfin's probe (saveHead); 0 disables
	username   string
	password   string

	apiClient = &http.Client{Timeout: 30 * time.Second}
	cdnClient = &http.Client{Transport: cdnTransport()}

	tokenMu sync.Mutex
	token   string

	memMu sync.Mutex
	mem   = map[string]memEntry{}

	urlMu sync.Mutex
	urls  = map[int]urlEntry{}

	// fetchMu holds one lock per listing key, so concurrent requests for a listing share one fetch while
	// other listings (and their disk reads) don't wait behind it.
	fetchMu sync.Map                 // listing key -> *sync.Mutex
	shrunk  = map[string]time.Time{} // listing key -> when it was first seen shrunk; guarded by memMu

	lookupMu    sync.Mutex
	failed      = map[int]time.Time{} // failed playback lookups; guarded by lookupMu
	crawlFailed = map[int]time.Time{} // failed crawler checks, kept apart so they don't block playback; guarded by lookupMu

	slots chan struct{} // upstream lookups/fetches in flight, capped by -concurrency; set in main

	hiQ = make(chan struct{})
	loQ = make(chan struct{})
)

type memEntry struct {
	v  any
	at time.Time
}

type item struct {
	ID      int      `json:"media_id"`
	Name    string   `json:"media_name"`
	NameEn  string   `json:"media_name_en"`
	Year    int      `json:"media_year"`
	Created string   `json:"created_at"`
	Episode int      `json:"episode_index"`
	Genres  []string `json:"media_genres"`
	Poster  string   `json:"media_thumbnail_url"`
	Banner  string   `json:"media_banner_url"`
}

type showInfo struct {
	Seasons []int
	Plot    string `json:"media_description"`
}

// newRequest builds a request with the browser User-Agent. URLs partly come from upstream data, so an
// invalid one is an error, not a panic.
func newRequest(ctx context.Context, method, u string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

func login(hi bool) (string, error) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if token != "" {
		return token, nil
	}
	if b, err := os.ReadFile(filepath.Join(cacheDir, "token")); err == nil {
		token = string(b)
		return token, nil
	}
	wait(hi)
	log.Printf("api: login")
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := newRequest(context.Background(), "POST", apiBase+"/user/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var r struct {
		Status   string
		Metadata struct {
			AccessToken string `json:"access_token"`
		}
	}
	if err := doJSON(req, &r); err != nil {
		return "", err
	}
	if r.Metadata.AccessToken == "" {
		return "", errors.New("login failed")
	}
	token = r.Metadata.AccessToken
	return token, writeFile(filepath.Join(cacheDir, "token"), []byte(token))
}

func doJSON(req *http.Request, out any) error {
	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		// Usually a Cloudflare challenge page instead of JSON.
		return fmt.Errorf("%s %s: HTTP %s: %w", req.Method, req.URL.Path, resp.Status, err)
	}
	return nil
}

// api GETs path and decodes the response's "data" into out. It re-logs in once on auth errors.
// hi requests (playback) jump the rate-limit queue.
func api(path string, out any, hi bool) error {
	for attempt := 0; ; attempt++ {
		tok, err := login(hi)
		if err != nil {
			return err
		}
		wait(hi)
		log.Printf("api: GET %s", path)
		req, err := newRequest(context.Background(), "GET", apiBase+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		var r struct {
			Status string
			Data   json.RawMessage
			Error  struct{ Message string }
		}
		if err := doJSON(req, &r); err != nil {
			return err
		}
		if r.Status == "success" {
			return json.Unmarshal(r.Data, out)
		}
		msg := r.Error.Message
		if attempt == 0 && (strings.Contains(msg, "jwt") || strings.Contains(msg, "prijavljeni")) {
			tokenMu.Lock()
			token = ""
			os.Remove(filepath.Join(cacheDir, "token"))
			tokenMu.Unlock()
			continue
		}
		return fmt.Errorf("%s: %s", path, msg)
	}
}

// cached returns fetch()'s result, memoized in memory and on disk for refreshTTL.
// If a refresh fails, the stale copy is served.
func cached[T any](key string, fetch func() (T, error)) (T, error) {
	memMu.Lock()
	e, ok := mem[key]
	memMu.Unlock()
	if ok && time.Since(e.at) < refreshTTL {
		return e.v.(T), nil
	}
	mu, _ := fetchMu.LoadOrStore(key, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	memMu.Lock()
	e, ok = mem[key]
	memMu.Unlock()
	if ok && time.Since(e.at) < refreshTTL {
		return e.v.(T), nil
	}
	var v T
	file := filepath.Join(cacheDir, "json", key+".json")
	if st, err := os.Stat(file); err == nil {
		b, _ := os.ReadFile(file)
		if json.Unmarshal(b, &v) == nil {
			e, ok = memEntry{v, st.ModTime()}, true
			if time.Since(st.ModTime()) < refreshTTL {
				memMu.Lock()
				mem[key] = e
				memMu.Unlock()
				return v, nil
			}
		}
	}
	release := acquire(false)
	v, err := fetch()
	release()
	memMu.Lock()
	if err == nil && ok && entries(v)*10 < entries(e.v)*9 {
		if first, seen := shrunk[key]; !seen {
			shrunk[key] = time.Now()
			err = fmt.Errorf("%d entries, down from %d; holding the old list for up to %v", entries(v), entries(e.v), shrinkAccept)
		} else if time.Since(first) < shrinkAccept {
			err = fmt.Errorf("%d entries, down from %d since %s", entries(v), entries(e.v), first.Format(time.DateTime))
		} else {
			log.Printf("%s: accepting shrink to %d entries after %v", key, entries(v), shrinkAccept)
		}
	}
	if err == nil {
		delete(shrunk, key)
	}
	memMu.Unlock()
	if err != nil {
		if ok {
			log.Printf("refresh %s failed, serving stale: %v", key, err)
			// Keep serving the stale copy for failTTL instead of retrying on every listing.
			memMu.Lock()
			mem[key] = memEntry{e.v, time.Now().Add(failTTL - refreshTTL)}
			memMu.Unlock()
			return e.v.(T), nil
		}
		return v, err
	}
	b, _ := json.Marshal(v)
	writeFile(file, b)
	memMu.Lock()
	mem[key] = memEntry{v, time.Now()}
	memMu.Unlock()
	return v, nil
}

// entries counts a cached listing's entries for the shrink guard.
func entries(v any) int {
	switch v := v.(type) {
	case []item:
		return len(v)
	case showInfo:
		return len(v.Seasons)
	}
	return 0
}

// catalog lists all movies (typ 1) or shows (typ 2).
func catalog(typ int) ([]item, error) {
	return cached(catalogKey(typ), func() ([]item, error) {
		var all []item
		// 300 is the API's max page size.
		for off := 0; ; off += 300 {
			var page []item
			if err := api(fmt.Sprintf("/media?sortBy=1&genres=&type=%d&query=&limit=300&offset=%d", typ, off), &page, false); err != nil {
				return nil, err
			}
			all = append(all, page...)
			if len(page) < 300 {
				return all, nil
			}
		}
	})
}

// firstN returns the first n items, or all if n is 0 (the -limit-* flags).
func firstN(items []item, n int) []item {
	if n > 0 {
		return items[:min(n, len(items))]
	}
	return items
}

// peek returns a cached listing from memory or disk, however old, without fetching it.
func peek[T any](key string) (T, bool) {
	memMu.Lock()
	e, ok := mem[key]
	memMu.Unlock()
	if ok {
		return e.v.(T), true
	}
	var v T
	b, err := os.ReadFile(filepath.Join(cacheDir, "json", key+".json"))
	return v, err == nil && json.Unmarshal(b, &v) == nil
}

func catalogKey(typ int) string             { return fmt.Sprintf("catalog-%d", typ) }
func showKey(showID int) string             { return fmt.Sprintf("show-%d", showID) }
func episodesKey(showID, season int) string { return fmt.Sprintf("episodes-%d-%d", showID, season) }

func showMeta(showID int) (showInfo, error) {
	return cached(showKey(showID), func() (showInfo, error) {
		var d showInfo
		err := api(fmt.Sprintf("/media/single/%d?dont_count_view=true", showID), &d, false)
		return d, err
	})
}

func episodes(showID, season int) ([]item, error) {
	return cached(episodesKey(showID, season), func() ([]item, error) {
		var eps []item
		err := api(fmt.Sprintf("/media/episodes/%d/%d", showID, season), &eps, false)
		return eps, err
	})
}

// writeFile replaces name atomically: a concurrent reader (a listing) never sees it empty or half-written,
// and a failed write (a full disk) leaves the old content.
func writeFile(name string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // after a successful rename there's nothing left to remove
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}

func subFile(loc string) string {
	return filepath.Join(cacheDir, "subs", filepath.Base(loc))
}

func subtitle(loc string) ([]byte, error) {
	if b, err := os.ReadFile(subFile(loc)); err == nil {
		return b, nil
	}
	if _, err := fetchSubtitle(loc); err != nil {
		return nil, err
	}
	return os.ReadFile(subFile(loc))
}

// fetchSubtitle downloads loc into the cache, or revalidates the cached copy by ETag.
// It reports whether the cached content changed.
func fetchSubtitle(loc string) (bool, error) {
	defer acquire(false)()
	wait(false)
	log.Printf("sub: GET %s", loc)
	req, err := newRequest(context.Background(), "GET", subBase+url.PathEscape(loc), nil)
	if err != nil {
		return false, err
	}
	if etag, err := os.ReadFile(subFile(loc) + ".etag"); err == nil {
		req.Header.Set("If-None-Match", string(etag))
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("subtitle %s: %s", loc, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	old, _ := os.ReadFile(subFile(loc))
	writeFile(subFile(loc)+".etag", []byte(resp.Header.Get("ETag")))
	return !bytes.Equal(old, b), writeFile(subFile(loc), b)
}
