package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDoodURL(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/e/good":
			w.Write([]byte(`<script>$.get('/pass_md5/123-abc/tok42', function(d){})</script>`))
		case "/e/gone":
			w.Write([]byte(`<title>Video not found | DoodStream</title>`))
		case "/e/challenge":
			w.Write([]byte(`<title>Just a moment...</title>`))
		case "/e/captcha":
			w.Write([]byte(`<title>X - DoodStream.com</title><script src="//challenges.cloudflare.com/turnstile/v0/api.js"></script>`))
		case "/pass_md5/123-abc/tok42":
			if r.Referer() != srv.URL+"/e/good" {
				t.Errorf("pass_md5 referer %q", r.Referer())
			}
			w.Write([]byte("https://cdn.example/path/file~"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	doodMirror = srv.URL + "/e/"
	takes := 0
	noWait := func() error { takes++; return nil }

	u, err := doodURL("good", noWait)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^https://cdn\.example/path/file~[A-Za-z0-9]{10}\?token=tok42&expiry=\d{13}$`).MatchString(u) {
		t.Fatalf("bad url %q", u)
	}
	if _, err := doodURL("gone", noWait); !errors.Is(err, errVideoGone) {
		t.Fatalf("gone: %v", err)
	}
	if _, err := doodURL("challenge", noWait); err == nil || errors.Is(err, errVideoGone) {
		t.Fatalf("challenge page should be a transient error, got %v", err)
	}

	if _, err := doodURL("captcha", noWait); err == nil || errors.Is(err, errVideoGone) || !strings.Contains(err.Error(), "captcha") {
		t.Fatalf("captcha should be a transient captcha error, got %v", err)
	}
	// Only the working lookup reached pass_md5, the one request DoodStream counts.
	if takes != 1 {
		t.Fatalf("%d lookups took a DoodStream slot, want 1", takes)
	}
}

// sloflix spells the source name inconsistently ("DoodStream", "Doodstream"); both must use the embed fallback.
func TestResolveDoodSpelling(t *testing.T) {
	var size atomic.Int64
	size.Store(777777777)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/e/abc":
			w.Write([]byte(`<script>$.get('/pass_md5/1-2/tok', function(d){})</script>`))
		case strings.HasPrefix(r.URL.Path, "/pass_md5/"):
			w.Write([]byte(srv.URL + "/file~"))
		case strings.HasPrefix(r.URL.Path, "/file~"):
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", size.Load()))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("x"))
		default: // the sloflix API
			fmt.Fprintf(w, `{"status":"success","data":{"media_sources":[{"media_source":"https://do7go.com/e/abc","media_source_name":"SLOSubs (Doodstream)"}]}}`)
		}
	}))
	defer srv.Close()
	defer func(cdn, api *http.Client) { cdnClient, apiClient = cdn, api }(cdnClient, apiClient)
	cdnClient, apiClient = srv.Client(), srv.Client()
	apiBase, doodMirror = srv.URL, srv.URL+"/e/"
	cacheDir = t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "meta"), 0755)
	token, slots = "test", make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case loQ <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()
	if _, m, err := resolve(7, false); err != nil || m.Size != 777777777 {
		t.Fatalf("size=%d err=%v", m.Size, err)
	}
	// A file under minVideoSize is a broken upload: hidden like a title without a source.
	size.Store(65536)
	if _, _, err := resolve(8, false); err == nil || !strings.Contains(err.Error(), "broken upload") {
		t.Fatalf("tiny file: %v", err)
	}
	if _, err := info(8); err == nil {
		t.Fatal("tiny file is listed")
	}
	writeMeta(9, meta{Size: 65536}) // recorded as playable before minVideoSize existed
	if _, err := info(9); err == nil {
		t.Fatal("tiny file recorded earlier is listed")
	}
}
