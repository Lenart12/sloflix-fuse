package main

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
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
}

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
	entries := make([]fuse.DirEntry, len(children))
	for i, c := range children {
		entries[i] = fuse.DirEntry{Name: c.name, Mode: fuse.S_IFREG}
		if c.dir {
			entries[i].Mode = fuse.S_IFDIR
		}
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
		log.Printf("getattr %d: %v", v.id, err)
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
	known, _, _ := readMeta(v.id)
	resolve := func(force bool) (string, int64, error) {
		u, size, err := streamURL(v.id, force)
		if err == nil && size != known.Size {
			// Upstream file changed: drop the kernel's cached attrs so it stops clamping reads to the old size.
			known.Size = size
			v.NotifyContent(-1, 0)
		}
		return u, size, err
	}
	_, size, err := resolve(false)
	if err != nil {
		log.Printf("open %d: %v", v.id, err)
		return nil, 0, syscall.EIO
	}
	return &stream{size: size, resolve: resolve}, 0, 0
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
	mu      sync.Mutex
	size    int64
	resolve func(force bool) (string, int64, error)
	body    io.ReadCloser
	pos     int64
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
		u, size, err := s.resolve(attempt > 0)
		if err != nil {
			return err
		}
		s.size = size
		req, err := newRequest(context.Background(), "GET", u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Referer", referer)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
		resp, err := cdnClient.Do(req)
		if err != nil {
			log.Printf("stream: %v", err)
			continue
		}
		if resp.StatusCode == http.StatusPartialContent {
			s.body, s.pos = resp.Body, off
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
	if off >= s.size {
		return fuse.ReadResultData(nil), 0
	}
	dest = dest[:min(int64(len(dest)), s.size-off)]
	for attempt := 0; attempt < 2; attempt++ {
		if s.body == nil || off < s.pos || off-s.pos > skipMax {
			if err := s.open(off); err != nil {
				log.Printf("stream: %v", err)
				return nil, syscall.EIO
			}
		} else if off > s.pos {
			if err := s.withDeadline(func(r io.Reader) error { _, err := io.CopyN(io.Discard, r, off-s.pos); return err }); err != nil {
				s.close()
				continue
			}
			s.pos = off
		}
		var n int
		err := s.withDeadline(func(r io.Reader) (err error) { n, err = io.ReadFull(r, dest); return err })
		s.pos += int64(n)
		if err == nil {
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
// Items that can't be resolved are logged and left out.
func mediaFiles(id int, base string, mtime time.Time) []child {
	m, err := info(id)
	if err != nil {
		log.Printf("media %d: %v", id, err)
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

// catalogDir lists the catalog of typ, newest first, capped at limit entries if limit > 0.
func catalogDir(typ, limit int, itemDir func(it item, name string) func() ([]child, error)) func() ([]child, error) {
	return func() ([]child, error) {
		items, err := catalog(typ)
		if err != nil {
			return nil, err
		}
		if limit > 0 {
			items = items[:min(limit, len(items))]
		}
		seen := map[string]bool{}
		var out []child
		for _, it := range items {
			name := uniq(seen, title(it), it.ID)
			out = append(out, dirChild(name, parseTime(it.Created), itemDir(it, name)))
		}
		return out, nil
	}
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
		si, err := showMeta(show.ID)
		if err != nil {
			return nil, err
		}
		out := nfoChild("tvshow.nfo", "tvshow", show, si.Plot, mtime)
		for _, s := range si.Seasons {
			out = append(out, dirChild(fmt.Sprintf("Season %02d", s), mtime, seasonDir(show.ID, s, name)))
		}
		return out, nil
	}
}

func seasonDir(showID, season int, showName string) func() ([]child, error) {
	return func() ([]child, error) {
		eps, err := episodes(showID, season)
		if err != nil {
			return nil, err
		}
		// Look episodes up in parallel (bounded by -concurrency), so one dead CDN node doesn't stall the listing.
		seen := map[string]bool{}
		files := make([][]child, len(eps))
		var wg sync.WaitGroup
		for i, ep := range eps {
			base := uniq(seen, fmt.Sprintf("%s S%02dE%02d", showName, season, ep.Episode), ep.ID)
			wg.Go(func() { files[i] = mediaFiles(ep.ID, base, parseTime(ep.Created)) })
		}
		wg.Wait()
		return slices.Concat(files...), nil
	}
}

func newRoot(limitMovies, limitShows int) *dir {
	return &dir{mtime: startTime, list: func() ([]child, error) {
		return []child{
			dirChild("Movies", startTime, catalogDir(1, limitMovies, movieDir)),
			dirChild("Shows", startTime, catalogDir(2, limitShows, showDir)),
		}, nil
	}}
}
