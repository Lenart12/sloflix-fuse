package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
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
}
