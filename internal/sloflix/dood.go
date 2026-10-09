package sloflix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

var (
	// Many titles only carry a DoodStream embed link, often on a dead mirror domain. Video codes work on any
	// mirror, so embeds are opened here instead.
	// doodstream.com redirects to whichever mirror is currently live.
	doodMirror = "https://doodstream.com/e/"

	errVideoGone = errors.New("video not found on DoodStream")
	passMD5      = regexp.MustCompile(`/pass_md5/[^'"]+`)
	htmlTitle    = regexp.MustCompile(`<title>([^<]*)`)
)

// doodURL turns a DoodStream video code into a direct, tokenized MP4 URL, the same way the embed player does:
// the embed page names a /pass_md5/ path whose response is the file's base URL.
func doodURL(code string, take func() error) (string, error) {
	get := func(u, ref string) ([]byte, *http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		req, err := newRequest(ctx, "GET", u, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Referer", ref)
		resp, err := cdnClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("doodstream %s: %s", u, resp.Status)
		}
		return b, resp, err
	}
	// load opens the player page and returns its pass_md5 path and (redirected) URL.
	load := func() ([]byte, *url.URL, error) {
		log.Printf("dood: GET %s", code)
		page, resp, err := get(doodMirror+code, "https://www.sloflix.com/")
		if err != nil {
			return nil, nil, err
		}
		if p := passMD5.Find(page); p != nil {
			return p, resp.Request.URL, nil
		}
		// Only an explicit "Video not found" means deleted; anything else (anti-bot or rate-limit pages)
		// is transient, so the title isn't persisted as gone.
		var title string
		if m := htmlTitle.FindSubmatch(page); m != nil {
			title = string(m[1])
		}
		if strings.Contains(strings.ToLower(title), "video not found") {
			return nil, nil, errVideoGone
		}
		if bytes.Contains(page, []byte("turnstile")) {
			return nil, nil, fmt.Errorf("doodstream %s: asks for a captcha (Turnstile)", code)
		}
		return nil, nil, fmt.Errorf("doodstream %s: no pass_md5 in page (title %q)", code, title)
	}
	p, embed, err := load()
	if err != nil {
		return "", err
	}
	// Only the pass_md5 request counts toward DoodStream's limit (player pages, deleted and unknown videos
	// don't; measured 2026-10-09), so the slot is taken only now.
	waitStart := time.Now()
	if err := take(); err != nil {
		return "", err
	}
	if time.Since(waitStart) > time.Second { // the page's pass_md5 path may have gone stale while waiting
		if p, embed, err = load(); err != nil {
			return "", err
		}
	}
	base, _, err := get(embed.Scheme+"://"+embed.Host+string(p), embed.String())
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(base, []byte("https://")) {
		return "", fmt.Errorf("doodstream %s: unexpected pass_md5 response %.40q", code, base)
	}
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	random := make([]byte, 10)
	for i := range random {
		random[i] = letters[rand.IntN(len(letters))]
	}
	return fmt.Sprintf("%s%s?token=%s&expiry=%d", base, random, path.Base(string(p)), time.Now().UnixMilli()), nil
}
