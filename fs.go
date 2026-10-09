package main

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Forward gaps up to this size are read and discarded instead of opening a new connection.
const skipMax = 1 << 20

// readTimeout bounds one read from the CDN. A stalled connection is closed and reopened instead of hanging
// the read (and Release, which waits for it) until TCP gives up.
var readTimeout = 30 * time.Second

var startTime = time.Now()

type child struct {
	name string
	dir  bool
	node func() (fs.InodeEmbedder, error)
	// visible, if set, decides whether the child is listed. Readdir checks every child, Lookup only the one
	// it found, so looking up one title doesn't read every title's meta.
	visible func() bool
}

func (c child) listed() bool { return c.visible == nil || c.visible() }

// dir is a read-only directory whose entries are produced on demand by list.
type dir struct {
	fs.Inode
	mtime time.Time
	list  func() ([]child, error)
}

func (d *dir) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFDIR | 0555
	out.SetTimes(nil, &d.mtime, &d.mtime)
	return 0
}

func (d *dir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	children, err := d.list()
	if err != nil {
		log.Printf("readdir: %v", err)
		return nil, syscall.EIO
	}
	var entries []fuse.DirEntry
	for _, c := range children {
		if !c.listed() {
			continue
		}
		e := fuse.DirEntry{Name: c.name, Mode: fuse.S_IFREG}
		if c.dir {
			e.Mode = fuse.S_IFDIR
		}
		entries = append(entries, e)
	}
	return fs.NewListDirStream(entries), 0
}

func (d *dir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	children, err := d.list()
	if err != nil {
		log.Printf("lookup %s: %v", name, err)
		return nil, syscall.EIO
	}
	for _, c := range children {
		if c.name != name {
			continue
		}
		if !c.listed() {
			return nil, syscall.ENOENT
		}
		n, err := c.node()
		if err != nil {
			log.Printf("lookup %s: %v", name, err)
			return nil, syscall.EIO
		}
		var a fuse.AttrOut
		if errno := n.(fs.NodeGetattrer).Getattr(ctx, nil, &a); errno != 0 {
			return nil, errno
		}
		out.Attr = a.Attr
		out.SetAttrTimeout(a.Timeout())
		return d.NewInode(ctx, n, fs.StableAttr{Mode: a.Mode & syscall.S_IFMT}), 0
	}
	return nil, syscall.ENOENT
}

type video struct {
	fs.Inode
	id    int
	mtime time.Time
}

func (v *video) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	m, err := info(v.id)
	if err != nil {
		if !quiet(err) {
			log.Printf("getattr %d: %v", v.id, err)
		}
		return syscall.EIO
	}
	mtime := mtimeOf(v.mtime, m)
	out.Mode = fuse.S_IFREG | 0444
	out.Size = uint64(m.Size)
	out.SetTimes(nil, &mtime, &mtime)
	return 0
}

func (v *video) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	log.Printf("open %d: %s", v.id, v.Path(nil))
	s := &stream{
		id:     v.id,
		opened: time.Now(),
		resolve: func(force bool) (string, error) {
			u, _, err := streamURL(v.id, force)
			return u, err
		},
		resized: func(size int64) {
			setSize(v.id, size)
			// Drop the kernel's cached attrs so it stops clamping reads to the old size. Async: invalidating
			// from inside the read that's still in flight on this inode could deadlock.
			go v.NotifyContent(-1, 0)
		},
	}
	// A new title's start, saved by the crawler: Jellyfin's probe is served from it without resolving a link.
	if m, err := info(v.id); err == nil {
		if f, err := os.Open(headFile(v.id)); err == nil {
			if st, err := f.Stat(); err == nil {
				s.size, s.head, s.headLen = m.Size, f, st.Size()
				return s, 0, 0
			}
			f.Close()
		}
	}
	_, size, err := streamURL(v.id, false)
	if err != nil {
		if !quiet(err) {
			log.Printf("open %d: %v", v.id, err)
		}
		return nil, 0, syscall.EIO
	}
	s.size = size
	return s, 0, 0
}

// mtimeOf bumps an item's mtime when its source or subtitle changed, so Jellyfin re-probes it.
func mtimeOf(created time.Time, m meta) time.Time {
	if m.Changed.After(created) {
		return m.Changed
	}
	return created
}

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
		req, err := newRequest(context.Background(), "GET", u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Referer", referer)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
		resp, err := cdnDo(req)
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
		os.Remove(headFile(s.id))
	}
	if s.conns > 0 || s.cached > 0 {
		log.Printf("close %d: %d connections, %.1f MB in %v, %.1f MB from probe cache", s.id, s.conns, float64(s.read)/1e6, time.Since(s.opened).Round(100*time.Millisecond), float64(s.cached)/1e6)
	}
	return 0
}

func subtitleNode(loc string, mtime time.Time) func() (fs.InodeEmbedder, error) {
	return func() (fs.InodeEmbedder, error) {
		b, err := subtitle(loc)
		if err != nil {
			return nil, err
		}
		f := &fs.MemRegularFile{Data: b}
		f.Attr.Mode = 0444
		f.Attr.SetTimes(nil, &mtime, &mtime)
		return f, nil
	}
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.DateTime, s)
	if err != nil {
		return startTime
	}
	return t
}

// name is the item's folder/file name: English if available (it matches TMDB best).
func name(it item) string {
	if it.NameEn != "" {
		return clean(it.NameEn)
	}
	return clean(it.Name)
}

// clean removes characters unsafe in file names. Some upstream titles carry invisible zero-width characters.
func clean(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' {
			return '-'
		}
		if !unicode.IsGraphic(r) {
			return -1
		}
		return r
	}, name)
	return strings.TrimSpace(name)
}

// title is the Jellyfin folder name: "Name (Year)".
func title(it item) string {
	if it.Year > 0 {
		return fmt.Sprintf("%s (%d)", name(it), it.Year)
	}
	return name(it)
}

type nfoThumb struct {
	Aspect string `xml:"aspect,attr,omitempty"`
	URL    string `xml:",chardata"`
}

// dataImage turns a "data:image/<type>;base64,..." URI into a sidecar image file named base.<type>.
func dataImage(uri, base string, mtime time.Time) (child, bool) {
	typ, data, ok := strings.Cut(strings.TrimPrefix(uri, "data:image/"), ";base64,")
	if !ok || len(typ) == len(uri) {
		return child{}, false
	}
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return child{}, false
	}
	return child{name: base + "." + strings.Replace(typ, "jpeg", "jpg", 1), node: func() (fs.InodeEmbedder, error) {
		f := &fs.MemRegularFile{Data: b}
		f.Attr.Mode = 0444
		f.Attr.SetTimes(nil, &mtime, &mtime)
		return f, nil
	}}, true
}

// reachable reports whether an artwork URL's host accepts connections (remembered per host, see hostStatus).
// Jellyfin aborts a whole metadata refresh when an image download times out, so a dead host must not
// end up in an NFO.
func reachable(uri string) bool {
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

// nfoChild renders sloflix's metadata as a Jellyfin/Kodi NFO, so titles TMDB can't match still get a plot,
// genres and artwork. Jellyfin downloads http(s) image URLs itself; images sloflix embeds as data: URIs (which
// Jellyfin rejects in an NFO) become poster/fanart sidecar files instead.
func nfoChild(file, root string, it item, plot string, mtime time.Time) []child {
	// Title in Slovenian, as on sloflix. No originaltitle: sloflix only knows the Slovenian and English names,
	// not the original-language one, so it's left for TMDB to fill when Jellyfin matches the item.
	n := struct {
		XMLName xml.Name
		Title   string     `xml:"title"`
		Year    int        `xml:"year,omitempty"`
		Plot    string     `xml:"plot,omitempty"`
		Genres  []string   `xml:"genre"`
		Poster  *nfoThumb  `xml:"thumb,omitempty"`
		Fanart  []nfoThumb `xml:"fanart>thumb,omitempty"`
	}{XMLName: xml.Name{Local: root}, Title: clean(it.Name), Year: it.Year, Plot: plot, Genres: it.Genres}
	var out []child
	art := func(uri, base string) string {
		if strings.HasPrefix(uri, "http") {
			if !reachable(uri) {
				return ""
			}
			return uri
		}
		if c, ok := dataImage(uri, base, mtime); ok {
			out = append(out, c)
		}
		return ""
	}
	if u := art(it.Poster, "poster"); u != "" {
		n.Poster = &nfoThumb{Aspect: "poster", URL: u}
	}
	if u := art(it.Banner, "fanart"); u != "" {
		n.Fanart = []nfoThumb{{URL: u}}
	}
	return append(out, child{name: file, node: func() (fs.InodeEmbedder, error) {
		b, err := xml.MarshalIndent(n, "", "  ")
		if err != nil {
			return nil, err
		}
		f := &fs.MemRegularFile{Data: append([]byte(xml.Header), b...)}
		f.Attr.Mode = 0444
		f.Attr.SetTimes(nil, &mtime, &mtime)
		return f, nil
	}})
}

// uniq returns name, or name with " [id]" appended if it was already taken.
func uniq(seen map[string]bool, name string, id int) string {
	if seen[name] {
		name = fmt.Sprintf("%s [%d]", name, id)
	}
	seen[name] = true
	return name
}

// mediaFiles lists the video file and, if present, its subtitle for item id under base name.
// Items the crawler hasn't verified as playable are left out.
func mediaFiles(id int, base string, mtime time.Time) []child {
	m, err := info(id)
	if err != nil {
		return nil
	}
	out := []child{{name: base + ".mp4", node: func() (fs.InodeEmbedder, error) { return &video{id: id, mtime: mtime}, nil }}}
	if m.Sub != "" {
		out = append(out, child{name: base + ".sl.vtt", node: subtitleNode(m.Sub, mtimeOf(mtime, m))})
	}
	return out
}

func dirChild(name string, mtime time.Time, list func() ([]child, error)) child {
	return child{name: name, dir: true, node: func() (fs.InodeEmbedder, error) { return &dir{mtime: mtime, list: list}, nil }}
}

// catalogDir lists the catalog of typ, newest first, capped at limit entries if limit > 0. Only titles that
// pass visible are listed; they're checked lazily (see child.visible).
func catalogDir(typ, limit int, visible func(id int) bool, itemDir func(it item, name string) func() ([]child, error)) func() ([]child, error) {
	return func() ([]child, error) {
		items, _ := peek[[]item](catalogKey(typ)) // stale is fine: the crawler refreshes listings
		seen := map[string]bool{}
		var out []child
		for _, it := range firstN(items, limit) {
			name := uniq(seen, title(it), it.ID) // hidden titles still take their name, so names stay stable
			c := dirChild(name, parseTime(it.Created), itemDir(it, name))
			c.visible = func() bool { return visible(it.ID) }
			out = append(out, c)
		}
		return out, nil
	}
}

func movieVisible(id int) bool {
	_, err := info(id)
	return err == nil
}

// showVisible reports whether any of a show's episodes is playable. It reads cached listings only, so
// listing Shows never waits on upstream, even right after a restart before the crawler's first pass.
func showVisible(id int) bool {
	si, _ := peek[showInfo](showKey(id))
	for _, s := range si.Seasons {
		eps, _ := peek[[]item](episodesKey(id, s))
		for _, ep := range eps {
			if _, err := info(ep.ID); err == nil {
				return true
			}
		}
	}
	return false
}

func movieDir(it item, name string) func() ([]child, error) {
	created := parseTime(it.Created)
	return func() ([]child, error) {
		files := mediaFiles(it.ID, name, created)
		if files == nil {
			return nil, nil
		}
		m, _ := info(it.ID)
		return append(files, nfoChild("movie.nfo", "movie", it, m.Plot, mtimeOf(created, m))...), nil
	}
}

func showDir(show item, name string) func() ([]child, error) {
	mtime := parseTime(show.Created)
	return func() ([]child, error) {
		si, _ := peek[showInfo](showKey(show.ID))
		out := nfoChild("tvshow.nfo", "tvshow", show, si.Plot, mtime)
		for _, s := range si.Seasons {
			list := seasonDir(show.ID, s, name)
			if files, _ := list(); len(files) == 0 {
				continue // no playable episodes yet
			}
			out = append(out, dirChild(fmt.Sprintf("Season %02d", s), mtime, list))
		}
		return out, nil
	}
}

func seasonDir(showID, season int, showName string) func() ([]child, error) {
	return func() ([]child, error) {
		eps, _ := peek[[]item](episodesKey(showID, season))
		seen := map[string]bool{}
		var out []child
		for _, ep := range eps {
			base := uniq(seen, fmt.Sprintf("%s S%02dE%02d", showName, season, ep.Episode), ep.ID)
			out = append(out, mediaFiles(ep.ID, base, parseTime(ep.Created))...)
		}
		return out, nil
	}
}

func newRoot(limitMovies, limitShows int) *dir {
	return &dir{mtime: startTime, list: func() ([]child, error) {
		return []child{
			dirChild("Movies", startTime, catalogDir(1, limitMovies, movieVisible, movieDir)),
			dirChild("Shows", startTime, catalogDir(2, limitShows, showVisible, showDir)),
		}, nil
	}}
}
