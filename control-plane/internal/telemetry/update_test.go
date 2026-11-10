package telemetry

import (
	"context"
	"strings"
	"testing"
)

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.0", "0.4.0", -1},
		{"0.4.0", "0.3.0", 1},
		{"0.3.0", "0.3.1", -1},
		{"0.3.1", "0.3.0", 1},
		{"0.3.0", "0.3.0", 0},
		{"v0.3.0", "0.3.0", 0},     // v-prefix ignored
		{"v1.2.3", "v1.2.3", 0},    // both v-prefixed
		{"1.0.0", "0.9.9", 1},      // major dominates
		{"0.10.0", "0.9.0", 1},     // numeric, not lexical
		{"", "0.0.0", 0},           // malformed → 0
		{"garbage", "0.0.0", 0},    // malformed → 0
		{"1.2", "1.2.0", 0},        // missing patch → 0
		{"v0.4.0-rc1", "0.4.0", 0}, // pre-release stripped
	}
	for _, tc := range cases {
		if got := CompareSemver(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareSemver(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCheckerUpdateAvailable(t *testing.T) {
	c := &Checker{
		Fetch: func(context.Context) (Release, error) {
			return Release{Tag: "v0.4.0", URL: "https://github.com/tastyeffectco/sandboxd/releases/tag/v0.4.0"}, nil
		},
	}
	if _, _, err := c.Latest(context.Background()); err != nil {
		t.Fatalf("Latest: %v", err)
	}

	avail, latest, url := c.UpdateAvailable("0.3.0")
	if !avail {
		t.Error("expected update available for current=0.3.0 vs latest=0.4.0")
	}
	if latest != "v0.4.0" {
		t.Errorf("latest = %q", latest)
	}
	if url == "" {
		t.Error("changelog url should be populated")
	}

	// Current == latest: no update.
	avail2, _, _ := c.UpdateAvailable("0.4.0")
	if avail2 {
		t.Error("no update should be available when current == latest")
	}

	// Current newer than latest (dev build ahead of release): no update.
	avail3, _, _ := c.UpdateAvailable("0.5.0")
	if avail3 {
		t.Error("no update should be available when current > latest")
	}
}

func TestCheckerUpdateAvailableEmptyCacheIsSafe(t *testing.T) {
	c := &Checker{
		Fetch: func(context.Context) (Release, error) {
			return Release{}, context.DeadlineExceeded
		},
	}
	// Latest returns the error; UpdateAvailable must be best-effort false.
	if _, _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("expected fetch error")
	}
	avail, _, _ := c.UpdateAvailable("0.3.0")
	if avail {
		t.Error("empty/failed cache must yield update_available=false")
	}
}

func TestCheckerUpdateAvailableNonSemverCurrent(t *testing.T) {
	c := &Checker{
		Fetch: func(context.Context) (Release, error) {
			return Release{Tag: "v0.3.0", URL: "https://github.com/tastyeffectco/sandboxd/releases/tag/v0.3.0"}, nil
		},
	}
	if _, _, err := c.Latest(context.Background()); err != nil {
		t.Fatalf("Latest: %v", err)
	}

	// A bare commit hash, "dev" or a describe form was never pinned to a
	// release: it reports an update of kind "untagged" so the console can word
	// it softly, and the latest release info is surfaced for display.
	for _, cur := range []string{"e2ca6f6", "dev", "unknown", "v0.3.0-54-g83f2f7", "v0.2.9-3-gabc", "v0.3.0-dirty"} {
		u := c.Status(cur)
		if !u.Available || u.Kind != "untagged" {
			t.Errorf("current=%q: want available/untagged, got %+v", cur, u)
		}
		if u.Latest != "v0.3.0" || u.ChangelogURL == "" {
			t.Errorf("current=%q: latest info should be surfaced, got %+v", cur, u)
		}
	}
	// A genuinely older tagged build reports a normal release update…
	if u := c.Status("v0.2.9"); !u.Available || u.Kind != "release" {
		t.Errorf("v0.2.9 should report a release update vs v0.3.0, got %+v", u)
	}
	// …and the latest tag itself (or a newer one) reports nothing.
	for _, cur := range []string{"v0.3.0", "0.3.0", "v0.3.1"} {
		if u := c.Status(cur); u.Available || u.Kind != "" {
			t.Errorf("current=%q: no update expected, got %+v", cur, u)
		}
	}
}

func TestCheckerStatusCarriesNotes(t *testing.T) {
	body := "## What's Changed\n* a thing\n\n## Breaking changes\n* the API moved\n\n## Bug fixes\n* x"
	c := &Checker{
		Fetch: func(context.Context) (Release, error) {
			return Release{Tag: "v0.4.0", URL: "u", Body: body, PublishedAt: "2026-08-01T00:00:00Z"}, nil
		},
	}
	if _, _, err := c.Latest(context.Background()); err != nil {
		t.Fatalf("Latest: %v", err)
	}
	u := c.Status("v0.3.0")
	if u.Notes != body || u.PublishedAt != "2026-08-01T00:00:00Z" {
		t.Errorf("notes/published_at not carried: %+v", u)
	}
	if u.Breaking != "* the API moved" {
		t.Errorf("breaking = %q", u.Breaking)
	}
}

func TestCheckerCapsNotes(t *testing.T) {
	long := strings.Repeat("line of notes\n", 3000) // ~42 KB
	c := &Checker{
		Fetch: func(context.Context) (Release, error) { return Release{Tag: "v1.0.0", Body: long}, nil },
	}
	if _, _, err := c.Latest(context.Background()); err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if n := len(c.Status("v0.1.0").Notes); n > maxNotesBytes+8 {
		t.Errorf("notes not capped: %d bytes", n)
	}
}

func TestBreakingSection(t *testing.T) {
	cases := []struct {
		name, md, want string
	}{
		{"h2 heading",
			"## What's Changed\n* a\n\n## Breaking changes\n* moved X\n* dropped Y\n\n## Other\n* b",
			"* moved X\n* dropped Y"},
		{"h3 with emoji, ends at same level",
			"## Notes\n### ⚠️ Breaking\nThe port changed.\n### Fixes\n* z",
			"The port changed."},
		{"h3 ends at higher level",
			"### ⚠️ Breaking\nOne\n\nTwo\n## Next\nno",
			"One\n\nTwo"},
		{"nested lower-level headings stay inside",
			"## Breaking changes\n### API\n* a\n### Config\n* b\n## Fixes\n* c",
			"### API\n* a\n### Config\n* b"},
		{"bold line heading",
			"**What's new**\n* a\n\n**Breaking changes**\n* b\n\n**Fixes**\n* c",
			"* b"},
		{"bold line heading with colon ends at # heading",
			"**Breaking changes:**\n* b\n## Fixes\n* c",
			"* b"},
		{"none present", "## What's Changed\n* nothing breaking here", ""},
		{"mention in body is not a heading", "## Notes\n* this is a breaking change\n", ""},
		{"section at end of body", "## Fixes\n* a\n\n## Breaking changes\n* last one\n", "* last one"},
		{"empty", "", ""},
		{"crlf", "## Breaking\r\n* a\r\n## B\r\n* c", "* a"},
	}
	for _, tc := range cases {
		if got := BreakingSection(tc.md); got != tc.want {
			t.Errorf("%s: BreakingSection = %q, want %q", tc.name, got, tc.want)
		}
	}
}
