package sloflix

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Jellyfin can't watch a FUSE mount for changes, so the crawler asks it to scan the sloflix libraries
// when titles it verified are waiting: when the probe cache is half full (so new titles keep getting
// heads) or when no new titles are left to check.
var (
	jfURL, jfKey, jfPath string

	jfCooldown = time.Hour // a var for tests
	jfBusy     sync.Mutex  // one scan request at a time
	jfMu       sync.Mutex
	jfNew      int       // titles verified since the last scan; guarded by jfMu
	jfLast     time.Time // when the last scan was started; guarded by jfMu
)

// jfVerified counts a newly listed title and scans if the probe cache is half full.
func jfVerified() {
	jfMu.Lock()
	jfNew++
	jfMu.Unlock()
	if probeCache > 0 && headCount()*2 >= probeCache {
		go jellyfinScan()
	}
}

// jellyfinScan starts a scan of the libraries under jfPath, unless it's off, nothing new was verified,
// the last one was started under jfCooldown ago, or Jellyfin is already scanning.
func jellyfinScan() {
	if jfURL == "" || !jfBusy.TryLock() {
		return
	}
	defer jfBusy.Unlock()
	jfMu.Lock()
	n, last := jfNew, jfLast
	jfMu.Unlock()
	if n == 0 || time.Since(last) < jfCooldown {
		return
	}
	var tasks []struct{ Key, State string }
	if err := jfDo("GET", "/ScheduledTasks", &tasks); err != nil {
		log.Printf("jellyfin: %v", err)
		return
	}
	for _, t := range tasks {
		if t.Key == "RefreshLibrary" && t.State == "Running" {
			return
		}
	}
	var libs []struct {
		Name, ItemId, RefreshStatus string // RefreshStatus: Idle, Queued or Active
		Locations                   []string
	}
	if err := jfDo("GET", "/Library/VirtualFolders", &libs); err != nil {
		log.Printf("jellyfin: %v", err)
		return
	}
	var ids, names []string
	for _, l := range libs {
		for _, loc := range l.Locations {
			if loc == jfPath || strings.HasPrefix(loc, strings.TrimSuffix(jfPath, "/")+"/") {
				if l.RefreshStatus != "Idle" {
					return // already scanning
				}
				ids, names = append(ids, l.ItemId), append(names, l.Name)
				break
			}
		}
	}
	if len(ids) == 0 {
		log.Printf("jellyfin: no library under %s", jfPath)
		return
	}
	for _, id := range ids {
		err := jfDo("POST", "/Items/"+id+"/Refresh?Recursive=true&MetadataRefreshMode=Default&ImageRefreshMode=Default&ReplaceAllMetadata=false&ReplaceAllImages=false", nil)
		if err != nil {
			log.Printf("jellyfin: %v", err)
			return
		}
	}
	jfMu.Lock()
	jfNew -= n // titles verified meanwhile wait for the next scan
	jfLast = time.Now()
	jfMu.Unlock()
	log.Printf("jellyfin: scanning %s (%d new titles; %d of %d probe cache slots used)", strings.Join(names, ", "), n, headCount(), probeCache)
}

func jfDo(method, path string, out any) error {
	req, err := http.NewRequest(method, strings.TrimSuffix(jfURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", `MediaBrowser Token="`+jfKey+`"`)
	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, req.URL.Path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
