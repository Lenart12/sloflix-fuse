package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiBase   = "https://api.sloflix.com/v1"
	subBase   = "https://sloflix.com/subtitles/"
	userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"
	referer   = "https://player.sloflix.com/"
	urlTTL    = time.Hour
	failTTL   = time.Hour
	// A listing that shrinks by more than 10% is treated as an upstream glitch, so Jellyfin doesn't delete
	// the missing titles. Only a shrink that persists this long is accepted.
	shrinkAccept = 24 * time.Hour
)

var (
	cacheDir   string
	refreshTTL time.Duration
	metaTTL    time.Duration
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

	// ponytail: one global lock dedupes concurrent catalog/list fetches; fine since they only hit the
	// throttled API with a 30s timeout. Per-key in-flight tracking (like lookups) if that becomes a bottleneck.
	fetchMu sync.Mutex
	shrunk  = map[string]time.Time{} // listing key -> when it was first seen shrunk; guarded by fetchMu

	lookupMu sync.Mutex
	lookups  = map[int]chan struct{}{} // in-flight title lookups, closed when done; guarded by lookupMu
	failed   = map[int]time.Time{}     // guarded by lookupMu

	slots chan struct{} // upstream lookups/fetches in flight, capped by -concurrency; set in main

	hiQ = make(chan struct{})
	loQ = make(chan struct{})
)

// throttleLoop hands out one request slot per interval, preferring waiters on hiQ (playback) over loQ.
func throttleLoop(every time.Duration) {
	for range time.Tick(every) {
		select {
		case hiQ <- struct{}{}:
		default:
			select {
			case hiQ <- struct{}{}:
			case loQ <- struct{}{}:
			}
		}
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

func wait(hi bool) {
	if hi {
		<-hiQ
	} else {
		<-loQ
	}
}

type memEntry struct {
	v  any
	at time.Time
}

type urlEntry struct {
	url  string
	size int64
	at   time.Time
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

// meta is what we persist per playable item: size for getattr, subtitle for listing the .vtt.
// Size 0 means no playable source was found. Changed is when a re-resolve last saw Size or Sub differ.
type meta struct {
	Size    int64     `json:"size"`
	Sub     string    `json:"sub"`
	Plot    string    `json:"plot"`
	Changed time.Time `json:"changed"`
}

type showInfo struct {
	Seasons []int
	Plot    string `json:"media_description"`
}

// cdnTransport has dial/TLS timeouts (from the default transport), plus a response header timeout.
// No overall timeout: playback responses stream for hours.
func cdnTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
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
	return token, os.WriteFile(filepath.Join(cacheDir, "token"), []byte(token), 0600)
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
	fetchMu.Lock()
	defer fetchMu.Unlock()
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
	if err != nil {
		if ok {
			log.Printf("refresh %s failed, serving stale: %v", key, err)
			// Keep serving the stale copy for failTTL instead of retrying (under fetchMu) on every listing.
			memMu.Lock()
			mem[key] = memEntry{e.v, time.Now().Add(failTTL - refreshTTL)}
			memMu.Unlock()
			return e.v.(T), nil
		}
		return v, err
	}
	b, _ := json.Marshal(v)
	os.WriteFile(file, b, 0644)
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
	return cached(fmt.Sprintf("catalog-%d", typ), func() ([]item, error) {
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

func showMeta(showID int) (showInfo, error) {
	return cached(fmt.Sprintf("show-%d", showID), func() (showInfo, error) {
		var d showInfo
		err := api(fmt.Sprintf("/media/single/%d?dont_count_view=true", showID), &d, false)
		return d, err
	})
}

func episodes(showID, season int) ([]item, error) {
	return cached(fmt.Sprintf("episodes-%d-%d", showID, season), func() ([]item, error) {
		var eps []item
		err := api(fmt.Sprintf("/media/episodes/%d/%d", showID, season), &eps, false)
		return eps, err
	})
}

// resolve fetches a fresh stream URL for id, records its size and subtitle in meta, and memoizes the URL.
func resolve(id int, hi bool) (string, meta, error) {
	defer acquire(hi)()
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
		if doodCode == "" && strings.Contains(s.Name, "DoodStream") && (strings.HasPrefix(u.Path, "/e/") || strings.HasPrefix(u.Path, "/d/")) {
			doodCode, doodSub = path.Base(u.Path), s.Sub
		}
	}
	var doodErr error
	if stream == "" && doodCode != "" {
		if stream, doodErr = doodURL(doodCode); doodErr != nil && !errors.Is(doodErr, errVideoGone) {
			return "", meta{}, fmt.Errorf("media %d: %w", id, doodErr)
		}
		if doodSub != nil {
			m.Sub = *doodSub
		}
	}
	if stream != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// A 1-byte range rather than HEAD: some sources are presigned S3/R2 URLs, signed for GET only.
		req, err := newRequest(ctx, "GET", stream, nil)
		if err != nil {
			return "", meta{}, fmt.Errorf("media %d: %w", id, err)
		}
		req.Header.Set("Referer", referer)
		req.Header.Set("Range", "bytes=0-0")
		resp, err := cdnClient.Do(req)
		if err != nil {
			return "", meta{}, err
		}
		resp.Body.Close()
		_, total, _ := strings.Cut(resp.Header.Get("Content-Range"), "/")
		size, _ := strconv.ParseInt(total, 10, 64)
		if resp.StatusCode != http.StatusPartialContent || size <= 0 {
			return "", meta{}, fmt.Errorf("media %d: size probe %s, Content-Range %q", id, resp.Status, resp.Header.Get("Content-Range"))
		}
		m.Size = size
	}
	if prev, _, ok := readMeta(id); ok {
		m.Changed = prev.Changed
		if prev.Size != m.Size || prev.Sub != m.Sub {
			log.Printf("media %d changed: size %d -> %d, sub %q -> %q", id, prev.Size, m.Size, prev.Sub, m.Sub)
			m.Changed = time.Now()
		}
	}
	writeMeta(id, m)
	if stream == "" && doodErr != nil {
		return "", m, fmt.Errorf("media %d: no direct source: DoodStream %s: %w", id, doodCode, doodErr)
	}
	if stream == "" {
		var got []string
		for _, s := range d.Sources {
			u, _ := url.Parse(s.Source)
			got = append(got, fmt.Sprintf("%s@%s", s.Name, u.Host))
		}
		return "", m, fmt.Errorf("media %d: no direct source (got %d: %v)", id, len(d.Sources), got)
	}
	urlMu.Lock()
	urls[id] = urlEntry{stream, m.Size, time.Now()}
	urlMu.Unlock()
	return stream, m, nil
}

var (
	// Many titles only carry a DoodStream embed link, often on a dead mirror domain. Video codes work on any
	// mirror, so embeds are opened here instead.
	doodMirror = "https://myvidplay.com/e/"

	errVideoGone = errors.New("video not found on DoodStream")
	passMD5      = regexp.MustCompile(`/pass_md5/[^'"]+`)
)

// doodURL turns a DoodStream video code into a direct, tokenized MP4 URL, the same way the embed player does:
// the embed page names a /pass_md5/ path whose response is the file's base URL.
func doodURL(code string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	get := func(u, ref string) ([]byte, *http.Response, error) {
		req, err := newRequest(ctx, "GET", u, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Referer", ref)
		resp, err := cdnClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("doodstream %s: %s", u, resp.Status)
		}
		return b, resp, err
	}
	log.Printf("dood: GET %s", code)
	page, resp, err := get(doodMirror+code, "https://www.sloflix.com/")
	if err != nil {
		return "", err
	}
	p := passMD5.Find(page)
	if p == nil {
		return "", errVideoGone
	}
	embed := resp.Request.URL // after the mirror's redirect
	base, _, err := get(embed.Scheme+"://"+embed.Host+string(p), embed.String())
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(base, []byte("https://")) {
		return "", fmt.Errorf("doodstream %s: unexpected pass_md5 response %.40q", code, base)
	}
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	random := make([]byte, 10)
	for i := range random {
		random[i] = letters[rand.IntN(len(letters))]
	}
	return fmt.Sprintf("%s%s?token=%s&expiry=%d", base, random, path.Base(string(p)), time.Now().UnixMilli()), nil
}

// info returns persisted meta for id, resolving it if unknown. Items without a source are an error,
// and are retried after refreshTTL.
func info(id int) (meta, error) {
	if m, ok := knownMeta(id); ok {
		return withSize(id, m)
	}
	lookupMu.Lock()
	if m, ok := knownMeta(id); ok {
		lookupMu.Unlock()
		return withSize(id, m)
	}
	if ch, busy := lookups[id]; busy {
		// Someone else is looking this title up (e.g. readdir and lookup racing): wait for their result.
		lookupMu.Unlock()
		<-ch
		if m, ok := knownMeta(id); ok {
			return withSize(id, m)
		}
		return meta{}, fmt.Errorf("media %d: lookup failed", id)
	}
	// Lookup failures (e.g. a dead CDN node) aren't persisted, so remember them briefly to avoid
	// retrying on every readdir/lookup/getattr.
	if time.Since(failed[id]) < failTTL {
		lookupMu.Unlock()
		return meta{}, fmt.Errorf("media %d: lookup failed recently", id)
	}
	ch := make(chan struct{})
	lookups[id] = ch
	lookupMu.Unlock()

	_, m, err := resolve(id, false)

	lookupMu.Lock()
	delete(lookups, id)
	if err != nil {
		failed[id] = time.Now()
	}
	lookupMu.Unlock()
	close(ch)
	if err != nil {
		return m, err
	}
	return withSize(id, m)
}

func withSize(id int, m meta) (meta, error) {
	if m.Size == 0 {
		return m, fmt.Errorf("media %d: no direct source", id)
	}
	return m, nil
}

// knownMeta returns persisted meta, unless it's missing or a no-source result older than refreshTTL.
func knownMeta(id int) (meta, bool) {
	m, written, ok := readMeta(id)
	return m, ok && (m.Size > 0 || time.Since(written) < refreshTTL)
}

func metaFile(id int) string {
	return filepath.Join(cacheDir, "meta", fmt.Sprint(id)+".json")
}

func writeMeta(id int, m meta) {
	b, _ := json.Marshal(m)
	os.WriteFile(metaFile(id), b, 0644)
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

// streamURL returns a memoized stream URL and its size, or fresh ones if force is set or it is older than urlTTL.
// Resolving also refreshes the item's persisted meta, so opening a file revalidates its size and subtitle.
func streamURL(id int, force bool) (string, int64, error) {
	urlMu.Lock()
	e, ok := urls[id]
	urlMu.Unlock()
	if ok && !force && time.Since(e.at) < urlTTL {
		return e.url, e.size, nil
	}
	u, m, err := resolve(id, true)
	return u, m.Size, err
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
	os.WriteFile(subFile(loc)+".etag", []byte(resp.Header.Get("ETag")), 0644)
	return !bytes.Equal(old, b), os.WriteFile(subFile(loc), b, 0644)
}

// refresher revalidates the least recently refreshed title, spread so each one comes up about every metaTTL.
// This catches changed or removed sources and subtitles added or edited upstream for titles nobody plays.
func refresher() {
	for {
		entries, _ := os.ReadDir(filepath.Join(cacheDir, "meta"))
		time.Sleep(max(metaTTL/time.Duration(len(entries)+1), 10*time.Second))
		var oldest int
		var oldestAt time.Time
		for _, e := range entries {
			fi, err := e.Info()
			if err != nil {
				continue
			}
			if oldest == 0 || fi.ModTime().Before(oldestAt) {
				fmt.Sscanf(e.Name(), "%d.json", &oldest)
				oldestAt = fi.ModTime()
			}
		}
		if oldest == 0 || time.Since(oldestAt) < metaTTL {
			continue
		}
		_, m, err := resolve(oldest, false)
		if err != nil {
			log.Printf("refresh: %v", err)
			// Push it to the back of the queue so one failing title doesn't block the rest.
			os.Chtimes(metaFile(oldest), time.Now(), time.Now())
			continue
		}
		if _, err := os.Stat(subFile(m.Sub)); m.Sub == "" || err != nil {
			continue
		}
		if changed, err := fetchSubtitle(m.Sub); err != nil {
			log.Printf("refresh: %v", err)
		} else if changed {
			log.Printf("media %d: subtitle %s changed", oldest, m.Sub)
			m.Changed = time.Now()
			writeMeta(oldest, m)
		}
	}
}
