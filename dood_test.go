package main

import (
	"errors"
	"fmt"
	"net"
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

	u, err := doodURL("good")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^https://cdn\.example/path/file~[A-Za-z0-9]{10}\?token=tok42&expiry=\d{13}$`).MatchString(u) {
		t.Fatalf("bad url %q", u)
	}
	if _, err := doodURL("gone"); !errors.Is(err, errVideoGone) {
		t.Fatalf("gone: %v", err)
	}
	if _, err := doodURL("challenge"); err == nil || errors.Is(err, errVideoGone) {
		t.Fatalf("challenge page should be a transient error, got %v", err)
	}

	if _, err := doodURL("captcha"); err == nil || errors.Is(err, errVideoGone) || !strings.Contains(err.Error(), "captcha") {
		t.Fatalf("captcha should be a transient captcha error, got %v", err)
	}
}

func TestProbeSize(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("error_nofile"))
			return
		}
		w.Header().Set("Content-Range", "bytes 0-0/12345")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("x"))
	}))
	var conns atomic.Int32
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	for range 2 {
		if n, err := probeSize(srv.URL + "/ok"); err != nil || n != 12345 {
			t.Fatalf("ok: %d %v", n, err)
		}
	}
	if conns.Load() != 1 {
		t.Fatalf("probe connections not reused: %d", conns.Load())
	}
	if _, err := probeSize(srv.URL + "/gone"); !errors.Is(err, errFileGone) {
		t.Fatalf("gone: %v", err)
	}

	// A video server that refused two connections in a row is skipped without another attempt.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	dead.Close()
	for i := range 2 {
		if _, err := probeSize("http://" + dead.Addr().String() + "/f"); err == nil || strings.Contains(err.Error(), "skipped") {
			t.Fatalf("probe %d of a dead server should try to connect: %v", i+1, err)
		}
	}
	if _, err := probeSize("http://" + dead.Addr().String() + "/f"); err == nil || !strings.Contains(err.Error(), "skipped") {
		t.Fatalf("third probe of a dead server should be skipped: %v", err)
	}
}

// sloflix spells the source name inconsistently ("DoodStream", "Doodstream"); both must use the embed fallback.
func TestResolveDoodSpelling(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/e/abc":
			w.Write([]byte(`<script>$.get('/pass_md5/1-2/tok', function(d){})</script>`))
		case strings.HasPrefix(r.URL.Path, "/pass_md5/"):
			w.Write([]byte(srv.URL + "/file~"))
		case strings.HasPrefix(r.URL.Path, "/file~"):
			w.Header().Set("Content-Range", "bytes 0-0/777")
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
	if _, m, err := resolve(7, false); err != nil || m.Size != 777 {
		t.Fatalf("size=%d err=%v", m.Size, err)
	}
}
