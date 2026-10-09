package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
		if strings.Contains(r.URL.Path, "expired") || r.Header.Get("Referer") == "" {
			http.Error(w, "gone", http.StatusForbidden)
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

	read(0, 1000) // expired URL -> 403 -> re-resolve -> 206
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
