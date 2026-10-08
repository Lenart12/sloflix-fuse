package main

import (
	"testing"
	"time"
)

func TestNfoArtwork(t *testing.T) {
	it := item{Name: "X", Poster: "https://img/p.jpg", Banner: "data:image/jpeg;base64,/9j/4A=="}
	var names []string
	for _, c := range nfoChild("movie.nfo", "movie", it, "", time.Time{}) {
		names = append(names, c.name)
	}
	if len(names) != 2 || names[0] != "fanart.jpg" || names[1] != "movie.nfo" {
		t.Fatalf("children = %v", names)
	}
	if _, ok := dataImage("https://img/p.jpg", "poster", time.Time{}); ok {
		t.Fatal("http URL treated as data URI")
	}
	if _, ok := dataImage("data:image/png;base64,!!", "poster", time.Time{}); ok {
		t.Fatal("bad base64 accepted")
	}
}
