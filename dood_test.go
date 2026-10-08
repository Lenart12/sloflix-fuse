package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestDoodURL(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/e/good":
			w.Write([]byte(`<script>$.get('/pass_md5/123-abc/tok42', function(d){})</script>`))
		case "/e/gone":
			w.Write([]byte(`<title>Video not found | DoodStream</title>`))
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

	u, err := doodURL("good")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^https://cdn\.example/path/file~[A-Za-z0-9]{10}\?token=tok42&expiry=\d{13}$`).MatchString(u) {
		t.Fatalf("bad url %q", u)
	}
	if _, err := doodURL("gone"); !errors.Is(err, errVideoGone) {
		t.Fatalf("gone: %v", err)
	}
}
