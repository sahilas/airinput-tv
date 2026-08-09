package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Skins are data, not code: a layout that names a button the server does not
// know still loads, still looks right, and simply does nothing when pressed.
// That failure is invisible until someone plays a game and wonders why one
// diagonal is dead — which is exactly how "DOWN,_LEFT" survived in the n64
// layout. These tests make that class of typo fail the build instead.

const skinsDir = "public/skins"

type skinEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Blurb  string `json:"blurb"`
	Sticks int    `json:"sticks"`
}

type skinRegistry struct {
	Skins []skinEntry `json:"skins"`
}

var (
	// Matches both data-btn="A" and data-btns="UP,LEFT".
	btnAttrRe   = regexp.MustCompile(`data-btns?="([^"]*)"`)
	stickZoneRe = regexp.MustCompile(`id="(stick-[a-z-]*zone)"`)
)

func loadRegistry(t *testing.T) skinRegistry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(skinsDir, "skins.json"))
	if err != nil {
		t.Fatalf("read skins.json: %v", err)
	}
	var reg skinRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatalf("skins.json is not valid JSON: %v", err)
	}
	if len(reg.Skins) == 0 {
		t.Fatal("skins.json lists no skins; the picker would have nothing to show")
	}
	return reg
}

// Every name a layout uses must be one setButton actually acts on.
func TestSkinButtonNamesAreKnown(t *testing.T) {
	layouts, err := filepath.Glob(filepath.Join(skinsDir, "*", "layout.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(layouts) == 0 {
		t.Fatalf("no layouts found under %s", skinsDir)
	}

	for _, path := range layouts {
		skin := filepath.Base(filepath.Dir(path))
		t.Run(skin, func(t *testing.T) {
			html, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			seen := map[string]bool{}
			for _, m := range btnAttrRe.FindAllStringSubmatch(string(html), -1) {
				for _, name := range strings.Split(m[1], ",") {
					name = strings.TrimSpace(name)
					if name == "" {
						t.Errorf("empty button name in %q", m[0])
						continue
					}
					seen[name] = true
					if !knownButton(name) {
						t.Errorf("unknown button %q in %s — the TV ignores it, so this control does nothing.\n"+
							"valid names: %s", name, m[0], strings.Join(knownButtonNames(), " "))
					}
				}
			}
			if len(seen) == 0 {
				t.Error("layout defines no buttons at all")
			}
		})
	}
}

// A mistyped stick id is the same silent failure: nipplejs looks the element up
// by id, finds nothing, and the skin quietly has no analog stick.
func TestSkinStickZoneIDs(t *testing.T) {
	layouts, _ := filepath.Glob(filepath.Join(skinsDir, "*", "layout.html"))
	valid := map[string]bool{"stick-left-zone": true, "stick-right-zone": true}

	for _, path := range layouts {
		skin := filepath.Base(filepath.Dir(path))
		html, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range stickZoneRe.FindAllStringSubmatch(string(html), -1) {
			if !valid[m[1]] {
				t.Errorf("%s: stick zone id %q is not recognised; use stick-left-zone or stick-right-zone", skin, m[1])
			}
		}
	}
}

// The registry drives the in-app picker, so an entry pointing at a missing
// folder shows a layout that fails to load when tapped, and an unregistered
// folder is a layout nobody can reach.
func TestSkinRegistryMatchesFolders(t *testing.T) {
	reg := loadRegistry(t)

	registered := map[string]bool{}
	for _, s := range reg.Skins {
		if s.ID == "" || s.Name == "" {
			t.Errorf("skins.json entry needs both id and name: %+v", s)
			continue
		}
		if registered[s.ID] {
			t.Errorf("duplicate id %q in skins.json", s.ID)
		}
		registered[s.ID] = true

		for _, f := range []string{"layout.html", "style.css"} {
			p := filepath.Join(skinsDir, s.ID, f)
			if _, err := os.Stat(p); err != nil {
				t.Errorf("skins.json lists %q but %s is missing", s.ID, p)
			}
		}
	}

	entries, err := os.ReadDir(skinsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !registered[e.Name()] {
			t.Errorf("skin folder %q is not listed in skins.json, so it never appears in the picker", e.Name())
		}
	}
}

// `sticks` is shown to nobody at runtime, so nothing would catch it drifting.
// Checking it keeps the registry honest as documentation for skin authors.
func TestSkinRegistryStickCounts(t *testing.T) {
	reg := loadRegistry(t)
	for _, s := range reg.Skins {
		html, err := os.ReadFile(filepath.Join(skinsDir, s.ID, "layout.html"))
		if err != nil {
			continue // already reported by TestSkinRegistryMatchesFolders
		}
		got := len(stickZoneRe.FindAllString(string(html), -1))
		if got != s.Sticks {
			t.Errorf("%s: skins.json says sticks=%d but layout.html has %d stick zone(s)", s.ID, s.Sticks, got)
		}
	}
}

// Guards the refactor that made dpadIndex a map so the tests above could read
// the same list setButton uses.
func TestKnownButtonCoversDpadAndFaceButtons(t *testing.T) {
	for _, name := range []string{"UP", "DOWN", "LEFT", "RIGHT", "A", "B", "X", "Y", "L", "R", "L2", "R2", "SELECT", "START"} {
		if !knownButton(name) {
			t.Errorf("knownButton(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "_LEFT", "up", "Z", "C", "HOME"} {
		if knownButton(name) {
			t.Errorf("knownButton(%q) = true, want false", name)
		}
	}
}

func knownButtonNames() []string {
	var out []string
	for n := range dpadIndex {
		out = append(out, n)
	}
	for n := range buttonBits {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
