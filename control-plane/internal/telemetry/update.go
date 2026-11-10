package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultReleasesURL is the GitHub API endpoint for the latest published release.
const defaultReleasesURL = "https://api.github.com/repos/tastyeffectco/sandboxd/releases/latest"

// defaultCheckTTL is how long a fetched "latest release" result is trusted before
// Latest will fetch again.
const defaultCheckTTL = 6 * time.Hour

// CompareSemver compares two semantic versions and returns -1, 0, or 1 for
// a<b, a==b, a>b. A leading "v" is ignored, and pre-release/build metadata
// (anything after "-" or "+") is stripped. Missing or non-numeric components are
// treated as 0, so malformed input compares as "0.0.0" rather than erroring.
func CompareSemver(a, b string) int {
	av, bv := parseSemver(a), parseSemver(b)
	for i := 0; i < 3; i++ {
		switch {
		case av[i] < bv[i]:
			return -1
		case av[i] > bv[i]:
			return 1
		}
	}
	return 0
}

func parseSemver(v string) [3]int {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i >= 3 {
			break
		}
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n > 0 {
			out[i] = n
		}
	}
	return out
}

// maxNotesBytes caps the release body kept in memory and served to clients.
const maxNotesBytes = 16 << 10

// Release is one published GitHub release, as much of it as the update surfaces
// need: the tag, its page, the markdown notes, and the breaking-changes section
// extracted from those notes ("" when the release has none).
type Release struct {
	Tag         string
	URL         string
	Body        string
	PublishedAt string
	Breaking    string
}

// Update is the answer to "should this build see an update?", ready to be
// written into /v1/settings.
type Update struct {
	// Available is true when Latest is newer than the running build, or when
	// the running build is not a clean release tag at all (see Kind).
	Available bool
	// Kind qualifies Available: "release" when the build is an older tag,
	// "untagged" when it carries no comparable version ("dev", a bare commit
	// hash, a describe form like v0.3.6-2-gabc). Empty when no update is reported.
	Kind         string
	Latest       string
	ChangelogURL string
	Notes        string
	Breaking     string
	PublishedAt  string
}

// Checker fetches the latest published release (cached ~6h) and answers whether
// a newer version than the running build exists. It is best-effort: on any fetch
// error UpdateAvailable simply reports false. It is safe for concurrent use.
type Checker struct {
	// URL overrides the GitHub releases endpoint (tests point this elsewhere).
	URL string
	// Fetch overrides the network fetch entirely (used by tests).
	Fetch func(ctx context.Context) (Release, error)
	// TTL overrides the cache lifetime; zero means the 6h default.
	TTL time.Duration
	// Now overrides the clock (tests); zero-value means time.Now.
	Now func() time.Time

	mu        sync.Mutex
	haveData  bool
	rel       Release
	lastFetch time.Time
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return defaultCheckTTL
}

// Latest returns the latest release tag and its changelog/release URL, fetching
// from GitHub at most once per TTL. Callers run this from a background goroutine;
// the request-path UpdateAvailable never fetches.
func (c *Checker) Latest(ctx context.Context) (version, url string, err error) {
	c.mu.Lock()
	if c.haveData && c.now().Sub(c.lastFetch) < c.ttl() {
		r := c.rel
		c.mu.Unlock()
		return r.Tag, r.URL, nil
	}
	c.mu.Unlock()

	fetch := c.Fetch
	if fetch == nil {
		fetch = c.githubFetch
	}
	rel, ferr := fetch(ctx)
	if ferr != nil {
		return "", "", ferr
	}
	rel.Body = capNotes(rel.Body)
	if rel.Breaking == "" {
		rel.Breaking = BreakingSection(rel.Body)
	}

	c.mu.Lock()
	c.rel, c.haveData, c.lastFetch = rel, true, c.now()
	c.mu.Unlock()
	return rel.Tag, rel.URL, nil
}

// UpdateAvailable reports, from the cached result only (never the network),
// whether the latest release is newer than current. See Status for the rules.
func (c *Checker) UpdateAvailable(current string) (available bool, latest, changelogURL string) {
	u := c.Status(current)
	return u.Available, u.Latest, u.ChangelogURL
}

// Status answers, from the cached result only (never the network), whether
// current should see an update and with which notes. It is best-effort: with no
// cached result yet everything is zero. When a result is cached it always
// carries the latest version, URL and notes so callers can surface them.
//
// A clean release tag ("v0.3.0") compares by semver. Anything else — "dev", a
// bare commit hash, a describe form "v0.3.0-54-g…" — is a build that was never
// pinned to a release, so it cannot be compared; it reports Available with
// Kind "untagged" whenever a release exists, unless it names the latest tag.
func (c *Checker) Status(current string) Update {
	c.mu.Lock()
	have, rel := c.haveData, c.rel
	c.mu.Unlock()
	if !have || rel.Tag == "" {
		return Update{}
	}
	u := Update{Latest: rel.Tag, ChangelogURL: rel.URL, Notes: rel.Body, Breaking: rel.Breaking, PublishedAt: rel.PublishedAt}
	switch {
	case strings.TrimSpace(current) == rel.Tag:
	case !cleanSemver(current):
		u.Available, u.Kind = true, "untagged"
	case CompareSemver(rel.Tag, current) > 0:
		u.Available, u.Kind = true, "release"
	}
	return u
}

// cleanSemver reports whether v is exactly a release tag — an optional "v"
// followed by three numeric components and nothing else.
func cleanSemver(v string) bool {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func capNotes(body string) string {
	if len(body) <= maxNotesBytes {
		return body
	}
	cut := body[:maxNotesBytes]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + "\n…"
}

// BreakingSection returns the body of the first section of md whose heading
// mentions "breaking", up to the next heading of the same or higher level; ""
// when there is none. A heading is a "#"-line of any level or a line that is
// entirely bold ("**Breaking changes**"); bold headings rank below every "#"
// heading, so a bold section ends at the next bold line or any "#" heading.
func BreakingSection(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	start, level := -1, 0
	for i, ln := range lines {
		lvl, text, ok := headingLine(ln)
		if !ok {
			continue
		}
		if start < 0 {
			if strings.Contains(strings.ToLower(text), "breaking") {
				start, level = i+1, lvl
			}
			continue
		}
		if lvl <= level {
			return strings.TrimSpace(strings.Join(lines[start:i], "\n"))
		}
	}
	if start < 0 {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[start:], "\n"))
}

// headingLine parses a markdown heading: "## Text" → (2, "Text"), a bold-only
// line "**Text**" → (7, "Text"). ok is false for anything else.
func headingLine(ln string) (level int, text string, ok bool) {
	t := strings.TrimSpace(ln)
	if strings.HasPrefix(t, "#") {
		n := 0
		for n < len(t) && t[n] == '#' {
			n++
		}
		if n > 6 || n == len(t) || (t[n] != ' ' && t[n] != '\t') {
			return 0, "", false
		}
		return n, strings.TrimSpace(strings.TrimRight(t[n:], "#")), true
	}
	if len(t) > 4 && strings.HasPrefix(t, "**") && strings.HasSuffix(strings.TrimSuffix(t, ":"), "**") {
		inner := strings.TrimSuffix(strings.TrimSuffix(t, ":"), "**")[2:]
		if !strings.Contains(inner, "**") {
			return 7, strings.TrimSpace(inner), true
		}
	}
	return 0, "", false
}

func (c *Checker) githubFetch(ctx context.Context) (Release, error) {
	endpoint := c.URL
	if endpoint == "" {
		endpoint = defaultReleasesURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	var body struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		Body        string `json:"body"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return Release{}, err
	}
	return Release{Tag: body.TagName, URL: body.HTMLURL, Body: body.Body, PublishedAt: body.PublishedAt}, nil
}
