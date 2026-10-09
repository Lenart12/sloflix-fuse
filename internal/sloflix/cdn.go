package sloflix

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cdnTransport has dial/TLS timeouts (from the default transport), plus a response header timeout.
// No overall timeout: playback responses stream for hours.
func cdnTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	// Healthy video servers connect in ~100ms; a dead one shouldn't hold a lookup for long.
	t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	// Go's own verification would reject expired certificates before VerifyConnection runs, so it's
	// skipped and verifyCert does the full verification itself.
	t.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection:   func(cs tls.ConnectionState) error { return verifyCert(cs, nil) },
	}
	return t
}

// expiredOK is the only domain whose expired certificates are accepted: some DoodStream video servers
// still serve files under a *.cloudatacdn.com certificate that expired on 2026-08-01.
const expiredOK = ".cloudatacdn.com"

var expiredSeen sync.Map // host -> struct{}, to log each accepted expired certificate once

// verifyCert verifies the server's chain and hostname against roots (nil: the system's). For hosts under
// expiredOK, an expired certificate is accepted if it verifies as of its own expiry: same chain and
// hostname checks, only the expiry is waived.
func verifyCert(cs tls.ConnectionState, roots *x509.CertPool) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("tls: no server certificate")
	}
	leaf := cs.PeerCertificates[0]
	opts := x509.VerifyOptions{DNSName: cs.ServerName, Roots: roots, Intermediates: x509.NewCertPool()}
	for _, c := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := leaf.Verify(opts)
	var invalid x509.CertificateInvalidError
	if err == nil || !strings.HasSuffix(cs.ServerName, expiredOK) || !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
		return err
	}
	opts.CurrentTime = leaf.NotAfter
	if _, err := leaf.Verify(opts); err != nil {
		return err
	}
	if _, seen := expiredSeen.LoadOrStore(cs.ServerName, struct{}{}); !seen {
		log.Printf("cdn: accepting expired certificate of %s (expired %s)", cs.ServerName, leaf.NotAfter.Format(time.DateOnly))
	}
	return nil
}

// GetRange requests stream URL u from byte off on, unless its video server is remembered as down.
func GetRange(u string, off int64) (*http.Response, error) {
	req, err := newRequest(context.Background(), "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", referer)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
	return cdnDo(req)
}

// cdnDo sends req to a video server, failing fast while its host is remembered as down.
func cdnDo(req *http.Request) (*http.Response, error) {
	addr := hostPort(req.URL)
	if ok, known := hostStatus(addr); known && !ok {
		return nil, fmt.Errorf("video server %s failed recently, skipped", req.URL.Host)
	}
	resp, err := cdnClient.Do(req)
	hostResult(addr, err)
	return resp, err
}

var (
	hostMu sync.Mutex
	hostOK = map[string]hostCheck{} // host:port -> last connection outcome (artwork hosts, video servers)
)

type hostCheck struct {
	ok    bool
	at    time.Time
	fails int // consecutive failures
}

// hostStatus returns a host's remembered outcome while it's fresh (failTTL): a success, or two or more
// failures in a row. A single failure (e.g. a DNS blip) isn't remembered, so the next request tries again.
func hostStatus(addr string) (ok, known bool) {
	hostMu.Lock()
	defer hostMu.Unlock()
	c := hostOK[addr]
	return c.ok, (c.ok || c.fails >= 2) && time.Since(c.at) < failTTL
}

// hostResult records a connection outcome and returns the number of consecutive failures.
func hostResult(addr string, err error) int {
	hostMu.Lock()
	defer hostMu.Unlock()
	c := hostOK[addr]
	if err != nil {
		c.fails++
	} else {
		c.fails = 0
	}
	c.ok, c.at = err == nil, time.Now()
	hostOK[addr] = c
	return c.fails
}

func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return net.JoinHostPort(u.Hostname(), port)
}

var errFileGone = errors.New("file deleted from CDN (error_nofile)")

// minVideoSize is the smallest file treated as a video. Smaller ones are broken uploads (seen: 32 KB and
// 64 KB films, a 1.6 MB episode) that ffprobe can't read, so they're hidden like titles without a source.
const minVideoSize = 5 << 20

// probeSize returns a stream's size from a 1-byte range request. Not HEAD: some sources are presigned
// S3/R2 URLs, signed for GET only. A DoodStream CDN whose file is gone answers 200 with "error_nofile".
// With headID set, it instead requests the whole file and saves its start for that title (see saveHead).
func probeSize(stream string, headID int) (int64, error) {
	timeout, rng := 15*time.Second, "bytes=0-0"
	if headID != 0 {
		timeout, rng = 2*time.Minute, "bytes=0-" // ~16 MB for a long film, at ~650 kB/s per connection
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := newRequest(ctx, "GET", stream, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Referer", referer)
	req.Header.Set("Range", rng)
	resp, err := cdnDo(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, total, _ := strings.Cut(resp.Header.Get("Content-Range"), "/")
	size, _ := strconv.ParseInt(total, 10, 64)
	if resp.StatusCode == http.StatusPartialContent && size > 0 {
		if headID == 0 {
			io.Copy(io.Discard, resp.Body) // read the 1 byte so the connection is reused for the stream that follows
		} else if err := saveHead(headID, resp.Body); err != nil {
			log.Printf("media %d: no probe cache: %v", headID, err) // Jellyfin's probe streams it instead
		}
		return size, nil
	}
	if b, _ := io.ReadAll(io.LimitReader(resp.Body, 64)); bytes.Contains(b, []byte("error_nofile")) {
		return 0, errFileGone
	}
	return 0, fmt.Errorf("size probe %s, Content-Range %q", resp.Status, resp.Header.Get("Content-Range"))
}

const (
	// headSlack is what Jellyfin's probe reads past the MP4 index: Jellyfin passes ffprobe no -probesize,
	// so its default of 5 MB applies, plus up to 1 MiB of kernel readahead (MaxReadAhead), plus margin.
	headSlack = 7 << 20
	headMax   = 64 << 20 // an index this far in means it isn't a sane MP4 start
	headTTL   = 3 * 24 * time.Hour
)

// HeadFile is where a title's probe head is saved (see saveHead).
func HeadFile(id int) string { return filepath.Join(cacheDir, "heads", fmt.Sprint(id)) }

// headCount is the number of saved heads, bounded by -probe-cache so they can't fill the disk before
// Jellyfin's next scan probes (and removes) them.
func headCount() int {
	heads, _ := os.ReadDir(filepath.Join(cacheDir, "heads"))
	return len(heads)
}

// saveHead saves an MP4's start, through its index (the moov box) plus headSlack, to HeadFile(id), so
// Jellyfin's probe of a new title is served from disk without a stream link (stream.Read). Files with
// the index at the end get none.
func saveHead(id int, r io.Reader) error {
	f, err := os.CreateTemp(filepath.Join(cacheDir, "heads"), "tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // after a successful rename there's nothing left to remove
	defer f.Close()
	for off := int64(0); ; {
		var hdr [16]byte
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return err
		}
		size, n := int64(binary.BigEndian.Uint32(hdr[:4])), int64(8)
		if size == 1 { // 64-bit size
			if _, err := io.ReadFull(r, hdr[8:]); err != nil {
				return err
			}
			size, n = int64(binary.BigEndian.Uint64(hdr[8:])), 16
		}
		typ := string(hdr[4:8])
		if typ == "mdat" || size < n || size > headMax-off { // not off+size: a huge 64-bit size would overflow
			return fmt.Errorf("not an MP4 with its index (moov) at the start: %q box of %d bytes at %d", typ, size, off)
		}
		f.Write(hdr[:n])
		if _, err := io.CopyN(f, r, size-n); err != nil {
			return err
		}
		if off += size; typ == "moov" {
			break
		}
	}
	if _, err := io.CopyN(f, r, headSlack); err != nil && err != io.EOF {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), HeadFile(id))
}

// Reachable reports whether an artwork URL's host accepts connections (remembered per host, see hostStatus).
// Jellyfin aborts a whole metadata refresh when an image download times out, so a dead host must not
// end up in an NFO.
func Reachable(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.Hostname() == "" {
		return false
	}
	addr := hostPort(u)
	if ok, known := hostStatus(addr); known {
		return ok
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err == nil {
		conn.Close()
	}
	if fails := hostResult(addr, err); err != nil {
		log.Printf("artwork host %s unreachable (%d in a row), left out of NFOs: %v", addr, fails, err)
	}
	return err == nil
}
