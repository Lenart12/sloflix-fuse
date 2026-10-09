package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStream(t *testing.T) {
	data := make([]byte, 4<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	var requests, resolves atomic.Int32
	var fresh atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Referer") == "" {
			http.Error(w, "no referer", http.StatusForbidden)
			return
		}
		if strings.Contains(r.URL.Path, "expired") { // what the CDN sends for an expired link
			w.Write([]byte("error_expired"))
			return
		}
		http.ServeContent(w, r, "f.mp4", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	s := &stream{size: int64(len(data)), resolve: func(force bool) (string, error) {
		// First URL is "expired"; a forced re-resolve yields a working one.
		resolves.Add(1)
		if force {
			fresh.Store(true)
		}
		if fresh.Load() {
			return srv.URL + "/ok", nil
		}
		return srv.URL + "/expired", nil
	}}
	read := func(off int64, n int) {
		t.Helper()
		res, errno := s.Read(context.Background(), make([]byte, n), off)
		if errno != 0 {
			t.Fatalf("read %d: errno %v", off, errno)
		}
		got, _ := res.Bytes(nil)
		if want := data[off:min(off+int64(n), int64(len(data)))]; !bytes.Equal(got, want) {
			t.Fatalf("read %d: got %d bytes, mismatch", off, len(got))
		}
	}

	read(0, 1000) // expired URL -> 200 "error_expired" -> re-resolve -> 206
	if resolves.Load() != 2 || requests.Load() != 2 {
		t.Fatalf("re-resolve: resolves=%d requests=%d", resolves.Load(), requests.Load())
	}
	read(1000, 128<<10) // sequential: same connection
	read(500<<10, 1000) // small forward skip: same connection
	if requests.Load() != 2 {
		t.Fatalf("sequential/skip reopened connection: requests=%d", requests.Load())
	}
	read(100, 1000)                 // backward seek: new connection
	read(3<<20, 1000)               // big forward seek: new connection
	read(int64(len(data))-10, 4096) // read past EOF is truncated
	if requests.Load() != 4 {
		t.Fatalf("seeks: requests=%d", requests.Load())
	}
	if res, _ := s.Read(context.Background(), make([]byte, 10), int64(len(data))); res.Size() != 0 {
		t.Fatal("read at EOF should be empty")
	}
	s.Release(context.Background())
}

func TestStreamStall(t *testing.T) {
	readTimeout = 100 * time.Millisecond
	data := bytes.Repeat([]byte("x"), 1<<16)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			// First connection sends headers and a few bytes, then stalls.
			w.Header().Set("Content-Range", "bytes 0-65535/65536")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[:10])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		http.ServeContent(w, r, "f.mp4", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	s := &stream{size: int64(len(data)), resolve: func(bool) (string, error) { return srv.URL, nil }}
	res, errno := s.Read(context.Background(), make([]byte, 1000), 0)
	if errno != 0 || res.Size() != 1000 {
		t.Fatalf("stalled read not retried: errno=%v size=%d requests=%d", errno, res.Size(), requests.Load())
	}
	s.Release(context.Background())
}

// A new size on the first response is adopted; a new size after data was served fails instead of splicing files.
func TestStreamSizeChange(t *testing.T) {
	data := bytes.Repeat([]byte("y"), 1<<21)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.mp4", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	var resized int64
	s := &stream{size: 1000, resolve: func(bool) (string, error) { return srv.URL, nil }, resized: func(n int64) { resized = n }}
	if res, errno := s.Read(context.Background(), make([]byte, 4096), 0); errno != 0 || res.Size() != 4096 {
		t.Fatalf("first read: errno=%v size=%d", errno, res.Size())
	}
	if resized != int64(len(data)) || s.size != int64(len(data)) {
		t.Fatalf("resized=%d size=%d", resized, s.size)
	}
	data = data[:1<<20] // replaced upstream while open
	if _, errno := s.Read(context.Background(), make([]byte, 10), 10); errno == 0 {
		t.Fatal("reconnect to a different-size file should fail")
	}
	s.Release(context.Background())
}

// Reads within a cached head never resolve a link; a read past it streams; closing removes the head.
func TestStreamHead(t *testing.T) {
	data := make([]byte, 2<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.mp4", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	cacheDir = t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "heads"), 0755)
	os.WriteFile(headFile(9), data[:1<<20], 0644)
	f, _ := os.Open(headFile(9))
	var resolves atomic.Int32
	s := &stream{id: 9, size: int64(len(data)), head: f, headLen: 1 << 20, resolve: func(bool) (string, error) {
		resolves.Add(1)
		return srv.URL, nil
	}}
	read := func(off int64, n int) {
		t.Helper()
		res, errno := s.Read(context.Background(), make([]byte, n), off)
		got, _ := res.Bytes(nil)
		if errno != 0 || !bytes.Equal(got, data[off:off+int64(n)]) {
			t.Fatalf("read %d: errno %v, %d bytes", off, errno, len(got))
		}
	}
	read(0, 4096)
	read(1<<20-4096, 4096)
	if resolves.Load() != 0 {
		t.Fatalf("reads within the head resolved %d times", resolves.Load())
	}
	read(1<<20-100, 4096) // crosses the end of the head
	if resolves.Load() != 1 {
		t.Fatalf("read past the head: resolves=%d", resolves.Load())
	}
	s.Release(context.Background())
	if _, err := os.Stat(headFile(9)); !os.IsNotExist(err) {
		t.Fatalf("head not removed on close: %v", err)
	}
}

// A head is the old file's start: once a response shows the upstream file was replaced, it's not served.
func TestStreamHeadResized(t *testing.T) {
	data := bytes.Repeat([]byte("n"), 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.mp4", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	cacheDir = t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "heads"), 0755)
	os.WriteFile(headFile(9), bytes.Repeat([]byte("o"), 4096), 0644)
	f, _ := os.Open(headFile(9))
	s := &stream{id: 9, size: 2 << 20, head: f, headLen: 4096, resolve: func(bool) (string, error) { return srv.URL, nil }}
	if _, errno := s.Read(context.Background(), make([]byte, 100), 8192); errno != 0 || s.size != int64(len(data)) {
		t.Fatalf("first read past the head: errno=%v size=%d", errno, s.size)
	}
	res, errno := s.Read(context.Background(), make([]byte, 100), 0)
	if got, _ := res.Bytes(nil); errno != 0 || !bytes.Equal(got, data[:100]) {
		t.Fatalf("read within the old head: errno=%v got %q", errno, got[:min(len(got), 8)])
	}
	s.Release(context.Background())
}
