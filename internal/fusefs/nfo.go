package fusefs

import (
	"encoding/base64"
	"encoding/xml"
	"strings"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/Lenart12/sloflixfs/internal/sloflix"
)

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
func nfoChild(file, root string, it sloflix.Item, plot string, mtime time.Time) []child {
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
			if !sloflix.Reachable(uri) {
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
