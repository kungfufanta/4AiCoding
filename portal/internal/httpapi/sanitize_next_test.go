package httpapi

import "testing"

// P1: the ?next= deep link must never escape the origin. Only
// same-origin absolute paths survive; protocol-relative ("//host"),
// backslashes and control characters are dropped (auth.go
// sanitizeNext).
func TestSanitizeNext(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/invites/abc123?token=xyz", "/invites/abc123?token=xyz"},
		{"/orgs/my-org/projects", "/orgs/my-org/projects"},
		{"", ""},
		{"https://evil.example/phish", ""},
		{"//evil.example", ""},
		{"/\\evil", ""},
		{"/line\r\ninjected: header", ""},
		{"relative/path", ""},
		{"/ok?q=/still//fine", "/ok?q=/still//fine"},
	}
	for _, c := range cases {
		if got := sanitizeNext(c.in); got != c.want {
			t.Errorf("sanitizeNext(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
