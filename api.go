package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
)

// doodPace spaces the crawler's lookups evenly over the window, at its share of it: 5m10s / (15-3) = 25.8s.
// Bursts of 12 would be just as much within the limit, but leave the crawler idle (and the log silent) for
// minutes, and would be the first thing to break if DoodStream ever penalised bursts.
func doodPace() time.Duration { return doodWindow / (doodMax - doodReserve) }

// doodTake waits until a DoodStream lookup fits the window, then records it. A sliding window of 15 also
// stays within a fixed window or token bucket of that size, whichever DoodStream uses. Playback doesn't
// wait: with its reserve used up too, it fails at once rather than blocking a read for minutes.
func doodTake(hi bool) error {
	limit := doodMax
	if !hi {
		limit -= doodReserve
	}
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
		}
		if wait <= 0 {
			doodSent = append(doodSent, now)
			if !hi {
				doodLastLo = now
			}
			doodMu.Unlock()
			return nil
		}
		doodMu.Unlock()
		if hi {
			return fmt.Errorf("DoodStream lookup limit reached, free again in %v", wait.Round(time.Second))
		}
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
	Gone    time.Time `json:"gone,omitzero"` // when upstream first reported a listed title gone (see goneGrace)
	// DeadCode is a DoodStream ID reported deleted. IDs aren't reused, so while sloflix keeps returning it,
	// rechecks of a hidden title skip DoodStream.
	DeadCode string `json:"dead_code,omitempty"`
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
	// Healthy video servers connect in ~100ms; a dead one shouldn't hold a lookup for long.
	t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	// Go's own verification would reject expired certificates before VerifyConnection runs, so it's
	// skipped and verifyCert does the full verification itself.
	t.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection:   func(cs tls.ConnectionState) error { return verifyCert(cs, nil) },
	}
	return t
}

// expiredOK is the only domain whose expired certificates are accepted: some DoodStream video servers
// still serve files under a *.cloudatacdn.com certificate that expired on 2026-08-01.
const expiredOK = ".cloudatacdn.com"

var expiredSeen sync.Map // host -> struct{}, to log each accepted expired certificate once

// verifyCert verifies the server's chain and hostname against roots (nil: the system's). For hosts under
// expiredOK, an expired certificate is accepted if it verifies as of its own expiry: same chain and
// hostname checks, only the expiry is waived.
func verifyCert(cs tls.ConnectionState, roots *x509.CertPool) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("tls: no server certificate")
	}
	leaf := cs.PeerCertificates[0]
	opts := x509.VerifyOptions{DNSName: cs.ServerName, Roots: roots, Intermediates: x509.NewCertPool()}
	for _, c := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := leaf.Verify(opts)
	var invalid x509.CertificateInvalidError
	if err == nil || !strings.HasSuffix(cs.ServerName, expiredOK) || !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
		return err
	}
	opts.CurrentTime = leaf.NotAfter
	if _, err := leaf.Verify(opts); err != nil {
		return err
	}
	if _, seen := expiredSeen.LoadOrStore(cs.ServerName, struct{}{}); !seen {
		log.Printf("cdn: accepting expired certificate of %s (expired %s)", cs.ServerName, leaf.NotAfter.Format(time.DateOnly))
	}
	return nil
}

// cdnDo sends req to a video server, failing fast while its host is remembered as down.
func cdnDo(req *http.Request) (*http.Response, error) {
	addr := hostPort(req.URL)
	if ok, known := hostStatus(addr); known && !ok {
		return nil, fmt.Errorf("video server %s failed recently, skipped", req.URL.Host)
	}
	resp, err := cdnClient.Do(req)
	hostResult(addr, err)
	return resp, err
}

var (
	hostMu sync.Mutex
	hostOK = map[string]hostCheck{} // host:port -> last connection outcome (artwork hosts, video servers)
)

type hostCheck struct {
	ok    bool
	at    time.Time
	fails int // consecutive failures
}

// hostStatus returns a host's remembered outcome while it's fresh (failTTL): a success, or two or more
// failures in a row. A single failure (e.g. a DNS blip) isn't remembered, so the next request tries again.
func hostStatus(addr string) (ok, known bool) {
	hostMu.Lock()
	defer hostMu.Unlock()
	c := hostOK[addr]
	return c.ok, (c.ok || c.fails >= 2) && time.Since(c.at) < failTTL
}

// hostResult records a connection outcome and returns the number of consecutive failures.
func hostResult(addr string, err error) int {
	hostMu.Lock()
	defer hostMu.Unlock()
	c := hostOK[addr]
	if err != nil {
		c.fails++
	} else {
		c.fails = 0
	}
	c.ok, c.at = err == nil, time.Now()
	hostOK[addr] = c
	return c.fails
}

func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return net.JoinHostPort(u.Hostname(), port)
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

var errFileGone = errors.New("file deleted from CDN (error_nofile)")

// probeSize returns a stream's size from a 1-byte range request. Not HEAD: some sources are presigned
// S3/R2 URLs, signed for GET only. A DoodStream CDN whose file is gone answers 200 with "error_nofile".
// With headID set, it instead requests the whole file and saves its start for that title (see saveHead).
func probeSize(stream string, headID int) (int64, error) {
	timeout, rng := 15*time.Second, "bytes=0-0"
	if headID != 0 {
		timeout, rng = 2*time.Minute, "bytes=0-" // ~16 MB for a long film, at ~650 kB/s per connection
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := newRequest(ctx, "GET", stream, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Referer", referer)
	req.Header.Set("Range", rng)
	resp, err := cdnDo(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, total, _ := strings.Cut(resp.Header.Get("Content-Range"), "/")
	size, _ := strconv.ParseInt(total, 10, 64)
	if resp.StatusCode == http.StatusPartialContent && size > 0 {
		if headID == 0 {
			io.Copy(io.Discard, resp.Body) // read the 1 byte so the connection is reused for the stream that follows
		} else if err := saveHead(headID, resp.Body); err != nil {
			log.Printf("media %d: no probe cache: %v", headID, err) // Jellyfin's probe streams it instead
		}
		return size, nil
	}
	if b, _ := io.ReadAll(io.LimitReader(resp.Body, 64)); bytes.Contains(b, []byte("error_nofile")) {
		return 0, errFileGone
	}
	return 0, fmt.Errorf("size probe %s, Content-Range %q", resp.Status, resp.Header.Get("Content-Range"))
}

const (
	// headSlack is what Jellyfin's probe reads past the MP4 index: Jellyfin passes ffprobe no -probesize,
	// so its default of 5 MB applies, plus up to 1 MiB of kernel readahead (MaxReadAhead), plus margin.
	headSlack = 7 << 20
	headMax   = 64 << 20 // an index this far in means it isn't a sane MP4 start
	headTTL   = 3 * 24 * time.Hour
)

func headFile(id int) string { return filepath.Join(cacheDir, "heads", fmt.Sprint(id)) }

// headCount is the number of saved heads, bounded by -probe-cache so they can't fill the disk before
// Jellyfin's next scan probes (and removes) them.
func headCount() int {
	heads, _ := os.ReadDir(filepath.Join(cacheDir, "heads"))
	return len(heads)
}

// saveHead saves an MP4's start, through its index (the moov box) plus headSlack, to headFile(id), so
// Jellyfin's probe of a new title is served from disk without a stream link (stream.Read). Files with
// the index at the end get none.
func saveHead(id int, r io.Reader) error {
	f, err := os.CreateTemp(filepath.Join(cacheDir, "heads"), "tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // after a successful rename there's nothing left to remove
	defer f.Close()
	for off := int64(0); ; {
		var hdr [16]byte
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return err
		}
		size, n := int64(binary.BigEndian.Uint32(hdr[:4])), int64(8)
		if size == 1 { // 64-bit size
			if _, err := io.ReadFull(r, hdr[8:]); err != nil {
				return err
			}
			size, n = int64(binary.BigEndian.Uint64(hdr[8:])), 16
		}
		typ := string(hdr[4:8])
		if typ == "mdat" || size < n || size > headMax-off { // not off+size: a huge 64-bit size would overflow
			return fmt.Errorf("not an MP4 with its index (moov) at the start: %q box of %d bytes at %d", typ, size, off)
		}
		f.Write(hdr[:n])
		if _, err := io.CopyN(f, r, size-n); err != nil {
			return err
		}
		if off += size; typ == "moov" {
			break
		}
	}
	if _, err := io.CopyN(f, r, headSlack); err != nil && err != io.EOF {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), headFile(id))
}

var (
	// Many titles only carry a DoodStream embed link, often on a dead mirror domain. Video codes work on any
	// mirror, so embeds are opened here instead.
	// doodstream.com redirects to whichever mirror is currently live.
	doodMirror = "https://doodstream.com/e/"

	errVideoGone = errors.New("video not found on DoodStream")
	passMD5      = regexp.MustCompile(`/pass_md5/[^'"]+`)
	htmlTitle    = regexp.MustCompile(`<title>([^<]*)`)
)

// doodURL turns a DoodStream video code into a direct, tokenized MP4 URL, the same way the embed player does:
// the embed page names a /pass_md5/ path whose response is the file's base URL.
func doodURL(code string, take func() error) (string, error) {
	get := func(u, ref string) ([]byte, *http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
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
	// load opens the player page and returns its pass_md5 path and (redirected) URL.
	load := func() ([]byte, *url.URL, error) {
		log.Printf("dood: GET %s", code)
		page, resp, err := get(doodMirror+code, "https://www.sloflix.com/")
		if err != nil {
			return nil, nil, err
		}
		if p := passMD5.Find(page); p != nil {
			return p, resp.Request.URL, nil
		}
		// Only an explicit "Video not found" means deleted; anything else (anti-bot or rate-limit pages)
		// is transient, so the title isn't persisted as gone.
		var title string
		if m := htmlTitle.FindSubmatch(page); m != nil {
			title = string(m[1])
		}
		if strings.Contains(strings.ToLower(title), "video not found") {
			return nil, nil, errVideoGone
		}
		if bytes.Contains(page, []byte("turnstile")) {
			return nil, nil, fmt.Errorf("doodstream %s: asks for a captcha (Turnstile)", code)
		}
		return nil, nil, fmt.Errorf("doodstream %s: no pass_md5 in page (title %q)", code, title)
	}
	p, embed, err := load()
	if err != nil {
		return "", err
	}
	// Only the pass_md5 request counts toward DoodStream's limit (player pages, deleted and unknown videos
	// don't; measured 2026-10-09), so the slot is taken only now.
	waitStart := time.Now()
	if err := take(); err != nil {
		return "", err
	}
	if time.Since(waitStart) > time.Second { // the page's pass_md5 path may have gone stale while waiting
		if p, embed, err = load(); err != nil {
			return "", err
		}
	}
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

// info returns a title's persisted meta. Titles the crawler hasn't verified, or found without a source, are
// an error. A playable title stays listed through transient errors and upstream reporting it gone within
// goneGrace (resolve keeps its size), so Jellyfin doesn't drop it and its watch history over a glitch.
func info(id int) (meta, error) {
	m, _, _ := readMeta(id)
	if m.Size == 0 {
		return m, errNoSource
	}
	return m, nil
}

// due reports whether the crawler should check id now, and in which order: bucket 0 is never checked,
// 1 a title without a source or in its goneGrace period (rechecked after refreshTTL), 2 a playable one
// (rechecked after metaTTL). written is when its meta was last written.
func due(id int) (bucket int, written time.Time, ok bool) {
	lookupMu.Lock()
	recent := time.Since(crawlFailed[id]) < failTTL
	lookupMu.Unlock()
	if recent {
		return 0, time.Time{}, false
	}
	m, written, known := readMeta(id)
	switch {
	case !known:
		return 0, written, true
	case m.Size == 0 || !m.Gone.IsZero():
		return 1, written, time.Since(written) >= refreshTTL
	default:
		return 2, written, time.Since(written) >= metaTTL
	}
}

func metaFile(id int) string {
	return filepath.Join(cacheDir, "meta", fmt.Sprint(id)+".json")
}

func writeMeta(id int, m meta) {
	b, _ := json.Marshal(m)
	writeFile(metaFile(id), b)
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

// crawler checks titles in the background, so listings only show playable titles and never wait on
// upstream: new titles first (newest first), then rechecks (see due), oldest first. This also catches
// changed or removed sources and subtitles added or edited upstream for titles nobody plays. The rate
// limits pace it; DoodStream's leaves a reserve for playback (doodTake). Workers beyond -concurrency (which
// still caps requests in flight) can wait for DoodStream without holding up titles that don't need it.
func crawler(limitMovies, limitShows int) {
	for {
		queue := crawlQueue(limitMovies, limitShows)
		// Rebuild at most every 10 minutes, so newly listed titles don't wait behind a long recheck queue.
		next := time.Now().Add(10 * time.Minute)
		ids := make(chan int)
		var wg sync.WaitGroup
		for range cap(slots) + doodMax {
			wg.Go(func() {
				for id := range ids {
					crawl(id)
				}
			})
		}
		for _, id := range queue {
			if time.Now().After(next) {
				break
			}
			ids <- id
		}
		close(ids)
		wg.Wait()
		time.Sleep(time.Until(next))
	}
}

// crawlQueue walks the catalog (the only place its listings are refreshed; readdir serves them cached) and returns the titles
// due for a check, in order. It also removes probe caches Jellyfin never read.
func crawlQueue(limitMovies, limitShows int) []int {
	type task struct {
		id, bucket int
		at         time.Time
	}
	var tasks []task
	add := func(id int, created time.Time) {
		if bucket, written, ok := due(id); ok {
			if bucket == 0 {
				written = created
			}
			tasks = append(tasks, task{id, bucket, written})
		}
	}
	movies, err := catalog(1)
	if err != nil {
		log.Printf("crawl: %v", err)
	}
	for _, it := range firstN(movies, limitMovies) {
		add(it.ID, parseTime(it.Created))
	}
	shows, err := catalog(2)
	if err != nil {
		log.Printf("crawl: %v", err)
	}
	for _, show := range firstN(shows, limitShows) {
		si, err := showMeta(show.ID)
		if err != nil {
			log.Printf("crawl: show %d: %v", show.ID, err)
			continue
		}
		for _, s := range si.Seasons {
			eps, err := episodes(show.ID, s)
			if err != nil {
				log.Printf("crawl: show %d season %d: %v", show.ID, s, err)
			}
			for _, ep := range eps {
				add(ep.ID, parseTime(ep.Created))
			}
		}
	}
	slices.SortFunc(tasks, func(a, b task) int {
		if a.bucket != b.bucket {
			return a.bucket - b.bucket
		}
		if a.bucket == 0 {
			return b.at.Compare(a.at) // newest first
		}
		return a.at.Compare(b.at) // least recently checked first
	})
	ids, fresh := make([]int, len(tasks)), 0
	for i, t := range tasks {
		ids[i] = t.id
		if t.bucket == 0 {
			fresh++
		}
	}
	heads, _ := os.ReadDir(filepath.Join(cacheDir, "heads"))
	for _, e := range heads {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > headTTL {
			os.Remove(filepath.Join(cacheDir, "heads", e.Name()))
		}
	}
	if len(ids) > 0 {
		log.Printf("crawl: %d new, %d to recheck; %d of %d probe cache slots used", fresh, len(ids)-fresh, headCount(), probeCache)
	}
	return ids
}

// crawl checks one title, unless playback did meanwhile, and revalidates its cached subtitle.
func crawl(id int) {
	if _, _, ok := due(id); !ok {
		return
	}
	_, m, err := resolve(id, false)
	if err != nil {
		lookupMu.Lock()
		crawlFailed[id] = time.Now() // transient errors aren't persisted; don't retry for failTTL
		lookupMu.Unlock()
		log.Printf("crawl %d: %v", id, err)
		return
	}
	if _, err := os.Stat(subFile(m.Sub)); m.Sub == "" || err != nil {
		return
	}
	if changed, err := fetchSubtitle(m.Sub); err != nil {
		log.Printf("crawl %d: %v", id, err)
	} else if changed {
		log.Printf("media %d: subtitle %s changed", id, m.Sub)
		m.Changed = time.Now()
		writeMeta(id, m)
	}
}
