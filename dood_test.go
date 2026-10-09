package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
		if n, err := probeSize(srv.URL+"/ok", 0); err != nil || n != 12345 {
			t.Fatalf("ok: %d %v", n, err)
		}
	}
	if conns.Load() != 1 {
		t.Fatalf("probe connections not reused: %d", conns.Load())
	}
	if _, err := probeSize(srv.URL+"/gone", 0); !errors.Is(err, errFileGone) {
		t.Fatalf("gone: %v", err)
	}

	// A video server that refused two connections in a row is skipped without another attempt.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	dead.Close()
	for i := range 2 {
		if _, err := probeSize("http://"+dead.Addr().String()+"/f", 0); err == nil || strings.Contains(err.Error(), "skipped") {
			t.Fatalf("probe %d of a dead server should try to connect: %v", i+1, err)
		}
	}
	if _, err := probeSize("http://"+dead.Addr().String()+"/f", 0); err == nil || !strings.Contains(err.Error(), "skipped") {
		t.Fatalf("third probe of a dead server should be skipped: %v", err)
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

// A new title's probe saves the file's start through its index (moov) plus headSlack; a file with the
// index at the end gets none.
func TestSaveHead(t *testing.T) {
	box := func(typ string, n int) []byte {
		b := make([]byte, n)
		binary.BigEndian.PutUint32(b, uint32(n))
		copy(b[4:], typ)
		for i := 8; i < n; i++ {
			b[i] = byte(i)
		}
		return b
	}
	start := append(box("ftyp", 32), box("moov", 1<<20)...)
	files := map[string][]byte{
		"/start": slices.Concat(start, box("mdat", 20<<20)),
		"/end":   slices.Concat(box("ftyp", 32), box("mdat", 20<<20), box("moov", 1<<20)),
		// A 64-bit size near the maximum, then data: must be rejected, not copied until the timeout.
		"/huge": slices.Concat(box("ftyp", 32), []byte{0, 0, 0, 1, 'f', 'r', 'e', 'e', 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, make([]byte, 20<<20)),
		// Not an MP4 (Matroska's magic): its first "box size" is far over headMax.
		"/mkv": slices.Concat([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 20<<20)),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.mp4", time.Time{}, bytes.NewReader(files[r.URL.Path]))
	}))
	defer srv.Close()
	cacheDir = t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "heads"), 0755)

	if n, err := probeSize(srv.URL+"/start", 1); err != nil || n != int64(len(files["/start"])) {
		t.Fatalf("start: %d %v", n, err)
	}
	head, _ := os.ReadFile(headFile(1))
	if want := files["/start"][:len(start)+headSlack]; !bytes.Equal(head, want) {
		t.Fatalf("head is %d bytes, want the first %d", len(head), len(want))
	}
	if n, err := probeSize(srv.URL+"/end", 2); err != nil || n != int64(len(files["/end"])) {
		t.Fatalf("end: %d %v", n, err)
	}
	if _, err := os.Stat(headFile(2)); !os.IsNotExist(err) {
		t.Fatalf("index at the end saved a head: %v", err)
	}
	// Rejected from the box header, not after copying until EOF or the timeout.
	for _, path := range []string{"/huge", "/mkv"} {
		if err := saveHead(3, bytes.NewReader(files[path])); err == nil || !strings.Contains(err.Error(), "not an MP4") {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if left, _ := os.ReadDir(filepath.Join(cacheDir, "heads")); len(left) != 1 {
		t.Fatalf("heads dir has %d files, want 1 (no temp files left)", len(left))
	}
}
