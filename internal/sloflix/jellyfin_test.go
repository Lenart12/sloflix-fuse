package sloflix

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestJellyfinScan(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	state, status := "Idle", "Idle"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != `MediaBrowser Token="key"` {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/ScheduledTasks":
			fmt.Fprintf(w, `[{"Key":"RefreshLibrary","State":%q}]`, state)
		case "/Library/VirtualFolders":
			fmt.Fprintf(w, `[{"Name":"Movies","ItemId":"m","Locations":["/media/sloflix/library/Movies"],"RefreshStatus":%q},
				{"Name":"Shows","ItemId":"s","Locations":["/media/sloflix/library/Shows"],"RefreshStatus":"Idle"},
				{"Name":"Home","ItemId":"h","Locations":["/media/sloflix/library2"],"RefreshStatus":"Idle"}]`, status)
		default:
			posted = append(posted, r.Method+" "+r.URL.Path)
		}
	}))
	defer srv.Close()
	defer func(c time.Duration) { jfURL, jfCooldown = "", c }(jfCooldown)
	jfURL, jfKey, jfPath = srv.URL, "key", "/media/sloflix/library"
	scan := func(last time.Time) []string {
		jfMu.Lock()
		jfNew, jfLast = 1, last
		jfMu.Unlock()
		mu.Lock()
		posted = nil
		mu.Unlock()
		jellyfinScan()
		mu.Lock()
		defer mu.Unlock()
		return posted
	}

	if got := scan(time.Time{}); !slices.Equal(got, []string{"POST /Items/m/Refresh", "POST /Items/s/Refresh"}) || jfNew != 0 {
		t.Fatalf("scan posted %v, jfNew %d", got, jfNew)
	}
	if got := scan(time.Now()); got != nil {
		t.Fatalf("scan within the cooldown posted %v", got)
	}
	status = "Queued"
	if got := scan(time.Time{}); got != nil {
		t.Fatalf("scan while a library refreshes posted %v", got)
	}
	status, state = "Idle", "Running"
	if got := scan(time.Time{}); got != nil {
		t.Fatalf("scan during a full scan posted %v", got)
	}
}
