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
	var apiCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/e/") {
			apiCalls.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/e/") {
			w.Write([]byte(`<title>Video not found | DoodStream</title>`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"media_sources":[{"media_source":"https://dead.example/e/abc","media_source_name":"X (DoodStream)"}]}}`))
	}))
	defer srv.Close()
	apiBase, doodMirror = srv.URL, srv.URL+"/e/"
	cacheDir = t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "meta"), 0755)
	token, refreshTTL = "test", 0 // refreshTTL 0: every listing rechecks titles in their grace period
	slots, failed = make(chan struct{}, 1), map[int]time.Time{}
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

	writeMeta(1, meta{Size: 100})
	if _, m, err := resolve(1, false); err == nil || m.Size != 100 || m.Gone.IsZero() {
		t.Fatalf("first gone: size=%d gone=%v err=%v", m.Size, m.Gone, err)
	}
	if m, err := info(1); err != nil || m.Size != 100 {
		t.Fatalf("within grace should stay listed: size=%d err=%v", m.Size, err)
	}
	m, _, _ := readMeta(1)
	m.Gone = time.Now().Add(-goneGrace - time.Minute)
	writeMeta(1, m)
	if _, err := info(1); err == nil {
		t.Fatal("after grace should be hidden")
	}
	if m, _, _ := readMeta(1); m.Size != 0 {
		t.Fatalf("after grace size=%d, want 0", m.Size)
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
