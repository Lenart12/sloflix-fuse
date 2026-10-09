package main

import (
	"flag"
	"strings"
	"testing"
)

// Flags take their value from SLOFLIX_<NAME>, the command line overrides it, and -h names the variable.
func TestEnvDefaults(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cache := fs.Int("probe-cache", 500, "")
	user := fs.String("user", "", "")
	other := fs.Bool("allow-other", false, "")
	env := map[string]string{"SLOFLIX_PROBE_CACHE": "1000", "SLOFLIX_USER": "env-user", "SLOFLIX_ALLOW_OTHER": "true"}
	if err := envDefaults(fs, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"-user", "cli-user"}); err != nil {
		t.Fatal(err)
	}
	if *cache != 1000 || !*other || *user != "cli-user" {
		t.Fatalf("probe-cache=%d allow-other=%v user=%q", *cache, *other, *user)
	}
	if u := fs.Lookup("probe-cache").Usage; !strings.Contains(u, "SLOFLIX_PROBE_CACHE") {
		t.Fatalf("usage %q doesn't name the variable", u)
	}
	env["SLOFLIX_PROBE_CACHE"] = "lots"
	if err := envDefaults(flag.NewFlagSet("t", flag.ContinueOnError), func(k string) string { return env[k] }); err != nil {
		t.Fatal("a variable for an undefined flag should be ignored")
	}
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	fs2.Int("probe-cache", 500, "")
	if err := envDefaults(fs2, func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "SLOFLIX_PROBE_CACHE") {
		t.Fatalf("bad value: %v", err)
	}
}
