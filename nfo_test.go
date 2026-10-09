package main

import (
	"net"
	"testing"
	"time"
)

func TestNfoArtwork(t *testing.T) {
	it := item{Name: "X", Poster: "http://127.0.0.1:1/p.jpg", Banner: "data:image/jpeg;base64,/9j/4A=="}
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

func TestReachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !reachable("http://" + l.Addr().String() + "/a.jpg") {
		t.Fatal("listening host reported unreachable")
	}
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	dead.Close()
	if reachable("http://" + dead.Addr().String() + "/a.jpg") {
		t.Fatal("closed port reported reachable")
	}
	// One failure is rechecked right away; two in a row hold for failTTL.
	up := l.Addr().String()
	hostOK[up] = hostCheck{at: time.Now(), fails: 1}
	if !reachable("http://" + up + "/a.jpg") {
		t.Fatal("single failure not rechecked")
	}
	hostOK[up] = hostCheck{at: time.Now().Add(-2 * time.Minute), fails: 2}
	if reachable("http://" + up + "/a.jpg") {
		t.Fatal("two failures in a row should hold for failTTL")
	}
	if reachable("data:image/jpeg;base64,AA==") {
		t.Fatal("URL without host reported reachable")
	}
}
