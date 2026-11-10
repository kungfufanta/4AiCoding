// Package previewhost is the single place that builds and parses the
// hostnames sandboxd serves: sandbox previews, the console and the API.
//
// Two styles exist. "nested" (the default) puts every preview under a
// dedicated `preview.` label; "flat" puts every host one label under
// PREVIEW_DOMAIN so a single `*.<domain>` wildcard certificate covers
// them all, and an optional tag lets several instances share one domain:
//
//	                nested                        flat
//	preview   s-<id>-<port>.preview.<D>    s-<id>-<port>[--tag].<D>
//	console   console.<D>                  console[--tag].<D>
//	api       api.preview.<D>              api[--tag].<D>
package previewhost

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	StyleNested = "nested"
	StyleFlat   = "flat"

	// tagSep separates the tag from the host label. Two hyphens cannot
	// occur inside a preview label (`s-<id>-<port>`, ids are
	// alphanumeric) so the tag is always unambiguous.
	tagSep = "--"
)

var tagRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// ValidTag reports whether s is an acceptable PREVIEW_HOST_TAG: a
// lowercase DNS-label fragment, 1-31 characters. Empty is valid (no tag).
func ValidTag(s string) bool {
	return s == "" || tagRE.MatchString(s)
}

// ParseStyle normalises a PREVIEW_HOST_STYLE value; empty means nested.
func ParseStyle(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", StyleNested:
		return StyleNested, nil
	case StyleFlat:
		return StyleFlat, nil
	}
	return "", fmt.Errorf("PREVIEW_HOST_STYLE must be nested or flat (got %q)", s)
}

// Scheme describes how hostnames are laid out under Domain. The zero
// Style is treated as nested; Tag is ignored unless Style is flat.
type Scheme struct {
	Domain string
	Style  string
	Tag    string
}

// New builds a Scheme, validating style and tag.
func New(domain, style, tag string) (Scheme, error) {
	st, err := ParseStyle(style)
	if err != nil {
		return Scheme{}, err
	}
	if !ValidTag(tag) {
		return Scheme{}, fmt.Errorf("PREVIEW_HOST_TAG must match [a-z0-9][a-z0-9-]{0,30} (got %q)", tag)
	}
	if st != StyleFlat {
		tag = ""
	}
	return Scheme{Domain: domain, Style: st, Tag: tag}, nil
}

// Flat reports whether hosts sit directly under Domain.
func (s Scheme) Flat() bool { return s.Style == StyleFlat }

// tagSuffix is "--<tag>" in flat mode with a tag, else "".
func (s Scheme) tagSuffix() string {
	if s.Flat() && s.Tag != "" {
		return tagSep + s.Tag
	}
	return ""
}

// Suffix is what every preview host ends with: ".preview.<D>" (nested)
// or "[--tag].<D>" (flat). Used for stale-router-label checks.
func (s Scheme) Suffix() string {
	if s.Flat() {
		return s.tagSuffix() + "." + s.Domain
	}
	return ".preview." + s.Domain
}

// Preview returns the hostname of one exposed sandbox port.
func (s Scheme) Preview(id string, port int) string {
	return fmt.Sprintf("s-%s-%d%s", id, port, s.Suffix())
}

// Console returns the console hostname.
func (s Scheme) Console() string {
	return "console" + s.tagSuffix() + "." + s.Domain
}

// API returns the hostname the control-plane API is reached on through
// Traefik.
func (s Scheme) API() string {
	if s.Flat() {
		return "api" + s.tagSuffix() + "." + s.Domain
	}
	return "api.preview." + s.Domain
}

// Wildcard is a human-readable pattern covering every preview host,
// shown in settings as preview_base. Nested is a real DNS wildcard
// (`*.preview.<D>`). Flat previews share their parent with the console
// and API, so no single DNS wildcard names only them; the pattern
// `s-*-*[--tag].<D>` stands in and mirrors Preview's shape.
func (s Scheme) Wildcard() string {
	if s.Flat() {
		return "s-*-*" + s.Suffix()
	}
	return "*.preview." + s.Domain
}

// CookieDomain is the Domain attribute a shared preview-session cookie
// may be set on. Only the nested style has a parent that belongs to
// this instance alone (`.preview.<D>`); in flat mode the only parent is
// `.<D>`, which other instances (tags) and the console share, so no
// shared cookie domain exists and "" is returned — callers must scope
// the cookie to a single host instead.
func (s Scheme) CookieDomain() string {
	if s.Flat() {
		return ""
	}
	return ".preview." + s.Domain
}

// HostRegexp matches a preview host of this scheme, capturing id (1)
// and port (2), tolerating a trailing ":port". It is anchored and
// tag-exact: with a tag it rejects untagged hosts and other tags; with
// no tag it rejects tagged hosts.
func (s Scheme) HostRegexp() *regexp.Regexp {
	return regexp.MustCompile(`^s-([0-9A-Za-z]+)-([0-9]+)` + regexp.QuoteMeta(s.Suffix()) + `(?::\d+)?$`)
}

// Parse extracts the sandbox id and port from a preview host. The id is
// uppercased: browsers lowercase Host headers while sandbox ids are
// canonical uppercase ULIDs.
func (s Scheme) Parse(host string) (id, port string, ok bool) {
	m := s.HostRegexp().FindStringSubmatch(strings.TrimSpace(host))
	if m == nil {
		return "", "", false
	}
	return strings.ToUpper(m[1]), m[2], true
}
