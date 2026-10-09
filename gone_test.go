package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A listed title that upstream reports gone stays listed for goneGrace, then is hidden.
func TestGoneGrace(t *testing.T) {
	var apiCalls, doodCalls atomic.Int32
	var captcha atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/e/") {
			apiCalls.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/e/") && captcha.Load() {
			w.Write([]byte(`<title>X - DoodStream.com</title><script src="turnstile.js"></script>`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/e/") {
			doodCalls.Add(1)
			w.Write([]byte(`<title>Video not found | DoodStream</title>`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"media_sources":[{"media_source":"https://dead.example/e/abc","media_source_name":"X (DoodStream)"}]}}`))
	}))
	defer srv.Close()
	apiBase, doodMirror = srv.URL, srv.URL+"/e/"
	cacheDir = t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "meta"), 0755)
	token, refreshTTL = "test", 0 // refreshTTL 0: every crawl rechecks titles in their grace period
	slots, failed, crawlFailed = make(chan struct{}, 1), map[int]time.Time{}, map[int]time.Time{}
	stop := make(chan struct{})
	defer close(stop)
	go func() { // hand out rate-limit ticks freely
		for {
			select {
			case hiQ <- struct{}{}:
			case loQ <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()

	writeMeta(1, meta{Size: 100 << 20})
	if _, m, err := resolve(1, false); err == nil || m.Size != 100<<20 || m.Gone.IsZero() {
		t.Fatalf("first gone: size=%d gone=%v err=%v", m.Size, m.Gone, err)
	}
	crawl(1) // recheck within grace
	if m, err := info(1); err != nil || m.Size != 100<<20 {
		t.Fatalf("within grace should stay listed: size=%d err=%v", m.Size, err)
	}
	delete(crawlFailed, 1)
	captcha.Store(true) // a transient failure during the recheck
	crawl(1)
	if m, err := info(1); err != nil || m.Size != 100<<20 {
		t.Fatalf("transient failure within grace should stay listed: size=%d err=%v", m.Size, err)
	}
	captcha.Store(false)
	delete(crawlFailed, 1)
	m, _, _ := readMeta(1)
	m.Gone = time.Now().Add(-goneGrace - time.Minute)
	writeMeta(1, m)
	crawl(1)
	if _, err := info(1); err == nil {
		t.Fatal("after grace should be hidden")
	}
	if m, _, _ := readMeta(1); m.Size != 0 {
		t.Fatalf("after grace size=%d, want 0", m.Size)
	}
	// Grace rechecks asked DoodStream each time; once hidden, the known-dead ID isn't requested again.
	if n := doodCalls.Load(); n != 3 {
		t.Fatalf("DoodStream requests through grace = %d, want 3", n)
	}
	delete(crawlFailed, 1) // the hide was a lookup failure; recheck for real
	crawl(1)
	if _, err := info(1); err == nil || doodCalls.Load() != 3 {
		t.Fatalf("recheck of a hidden title: err=%v DoodStream requests=%d, want 3", err, doodCalls.Load())
	}

	// A failed playback lookup is remembered: retrying within playFailTTL makes no API call.
	delete(failed, 1)
	before := apiCalls.Load()
	for range 2 {
		if _, _, err := streamURL(1, false); err == nil {
			t.Fatal("streamURL of a gone title succeeded")
		}
	}
	if n := apiCalls.Load() - before; n != 1 {
		t.Fatalf("playback retries made %d API calls, want 1", n)
	}
}
