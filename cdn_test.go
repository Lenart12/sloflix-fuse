package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
