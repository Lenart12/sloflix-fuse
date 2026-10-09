package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/Lenart12/sloflixfs/internal/sloflix"
)

// Forward gaps up to this size are read and discarded instead of opening a new connection.
const skipMax = 1 << 20

// readTimeout bounds one read from the CDN. A stalled connection is closed and reopened instead of hanging
// the read (and Release, which waits for it) until TCP gives up.
var readTimeout = 30 * time.Second

// stream serves reads from a single HTTP Range response, reopening it only on seeks.
type stream struct {
	id      int
	mu      sync.Mutex
	size    int64
	resolve func(force bool) (string, error)
	resized func(size int64) // called when the first response shows the upstream file has a new size
	served  bool             // data was already returned, so a size change would splice two files
	body    io.ReadCloser
	pos     int64
	head    *os.File // the file's start, if cached for Jellyfin's probe (saveHead); removed on close
	headLen int64

	conns  int   // connections opened, for the summary logged on close
	read   int64 // bytes downloaded, including skipped gaps
	cached int64 // bytes served from head
	opened time.Time
}

func (s *stream) close() {
	if s.body != nil {
		s.body.Close()
		s.body = nil
	}
}

func (s *stream) open(off int64) error {
	s.close()
	for attempt := 0; attempt < 2; attempt++ {
		u, err := s.resolve(attempt > 0)
		if err != nil {
			return err
		}
		resp, err := sloflix.GetRange(u, off)
		if err != nil {
			log.Printf("stream: %v", err)
			continue
		}
		if resp.StatusCode == http.StatusPartialContent {
			_, total, _ := strings.Cut(resp.Header.Get("Content-Range"), "/")
			if size, _ := strconv.ParseInt(total, 10, 64); size > 0 && size != s.size {
				if s.served {
					resp.Body.Close()
					return fmt.Errorf("upstream file changed from %d to %d bytes while open", s.size, size)
				}
				s.size = size
				if s.head != nil { // it's the old file's start (resized removes the file)
					s.head.Close()
					s.head = nil
				}
				if s.resized != nil {
					s.resized(size)
				}
			}
			s.body, s.pos = resp.Body, off
			s.conns++
			return nil
		}
		resp.Body.Close()
		log.Printf("stream: %s, re-resolving", resp.Status)
	}
	return errors.New("stream unavailable")
}

func (s *stream) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reads must be whole (a short read means EOF), so only those entirely within head are served from it.
	if n := min(int64(len(dest)), s.size-off); s.head != nil && n > 0 && off+n <= s.headLen {
		if got, _ := s.head.ReadAt(dest[:n], off); got == int(n) {
			s.served = true
			s.cached += n
			return fuse.ReadResultData(dest[:n]), 0
		}
	}
	buf := dest
	for attempt := 0; attempt < 2; attempt++ {
		if off >= s.size {
			return fuse.ReadResultData(nil), 0
		}
		if s.body == nil || off < s.pos || off-s.pos > skipMax {
			if err := s.open(off); err != nil {
				log.Printf("stream: %v", err)
				return nil, syscall.EIO
			}
			if off >= s.size { // the first response may have shown a smaller file
				return fuse.ReadResultData(nil), 0
			}
		} else if off > s.pos {
			if err := s.withDeadline(func(r io.Reader) error { n, err := io.CopyN(io.Discard, r, off-s.pos); s.read += n; return err }); err != nil {
				s.close()
				continue
			}
			s.pos = off
		}
		dest := buf[:min(int64(len(buf)), s.size-off)]
		var n int
		err := s.withDeadline(func(r io.Reader) (err error) { n, err = io.ReadFull(r, dest); return err })
		s.pos += int64(n)
		s.read += int64(n)
		if err == nil {
			s.served = true
			return fuse.ReadResultData(dest), 0
		}
		log.Printf("stream: read at %d: %v", off, err)
		s.close()
	}
	return nil, syscall.EIO
}

// withDeadline runs fn on the open body, closing the body if fn takes longer than readTimeout.
func (s *stream) withDeadline(fn func(io.Reader) error) error {
	body := s.body
	t := time.AfterFunc(readTimeout, func() { body.Close() })
	defer t.Stop()
	return fn(body)
}

func (s *stream) Release(ctx context.Context) syscall.Errno {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.close()
	if s.head != nil {
		// Jellyfin probes a file once; later opens stream it.
		s.head.Close()
		os.Remove(sloflix.HeadFile(s.id))
	}
	if s.conns > 0 || s.cached > 0 {
		log.Printf("close %d: %d connections, %.1f MB in %v, %.1f MB from probe cache", s.id, s.conns, float64(s.read)/1e6, time.Since(s.opened).Round(100*time.Millisecond), float64(s.cached)/1e6)
	}
	return 0
}
