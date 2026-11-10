package previewhost

import "testing"

func TestHostnames(t *testing.T) {
	cases := []struct {
		name                                string
		s                                   Scheme
		preview, console, api, wild, suffix string
		cookie                              string
	}{
		{"nested", Scheme{Domain: "ex.com"},
			"s-ABC-3000.preview.ex.com", "console.ex.com", "api.preview.ex.com", "*.preview.ex.com", ".preview.ex.com", ".preview.ex.com"},
		{"nested-explicit", Scheme{Domain: "ex.com", Style: StyleNested, Tag: "ignored"},
			"s-ABC-3000.preview.ex.com", "console.ex.com", "api.preview.ex.com", "*.preview.ex.com", ".preview.ex.com", ".preview.ex.com"},
		{"flat", Scheme{Domain: "ex.com", Style: StyleFlat},
			"s-ABC-3000.ex.com", "console.ex.com", "api.ex.com", "s-*-*.ex.com", ".ex.com", ""},
		{"flat-tag", Scheme{Domain: "ex.com", Style: StyleFlat, Tag: "eu1"},
			"s-ABC-3000--eu1.ex.com", "console--eu1.ex.com", "api--eu1.ex.com", "s-*-*--eu1.ex.com", "--eu1.ex.com", ""},
	}
	for _, c := range cases {
		got := []string{c.s.Preview("ABC", 3000), c.s.Console(), c.s.API(), c.s.Wildcard(), c.s.Suffix(), c.s.CookieDomain()}
		want := []string{c.preview, c.console, c.api, c.wild, c.suffix, c.cookie}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s[%d]: got %q want %q", c.name, i, got[i], want[i])
			}
		}
	}
}

func TestParse(t *testing.T) {
	nested := Scheme{Domain: "ex.com"}
	flat := Scheme{Domain: "ex.com", Style: StyleFlat}
	tagged := Scheme{Domain: "ex.com", Style: StyleFlat, Tag: "eu1"}
	cases := []struct {
		s        Scheme
		host     string
		id, port string
		ok       bool
	}{
		{nested, "s-abc-3000.preview.ex.com", "ABC", "3000", true},
		{nested, "s-ABC-3000.preview.ex.com:8080", "ABC", "3000", true},
		{nested, "s-abc-3000.ex.com", "", "", false},
		{nested, "s-abc-3000--eu1.ex.com", "", "", false},
		{nested, "xs-abc-3000.preview.ex.com", "", "", false},
		{nested, "s-abc-3000.preview.ex.com.evil", "", "", false},
		{nested, "s-abc-3000.extra.preview.ex.com", "", "", false},
		{nested, "s-abc-3000.preview.ex.com:80x", "", "", false},
		{flat, "s-abc-3000.ex.com", "ABC", "3000", true},
		{flat, "s-abc-3000.ex.com:18080", "ABC", "3000", true},
		{flat, "s-abc-3000--eu1.ex.com", "", "", false},
		{flat, "s-abc-3000.preview.ex.com", "", "", false},
		{flat, "s-abc-3000.ex.com.evil", "", "", false},
		{tagged, "s-abc-3000--eu1.ex.com", "ABC", "3000", true},
		{tagged, "s-abc-3000--eu1.ex.com:443", "ABC", "3000", true},
		{tagged, "s-abc-3000--eu2.ex.com", "", "", false},
		{tagged, "s-abc-3000--eu.ex.com", "", "", false},
		{tagged, "s-abc-3000--eu1x.ex.com", "", "", false},
		{tagged, "s-abc-3000.ex.com", "", "", false},
		{tagged, "s-abc-3000--eu1.preview.ex.com", "", "", false},
		{tagged, "s-abc-3000.x--eu1.ex.com", "", "", false},
	}
	for _, c := range cases {
		id, port, ok := c.s.Parse(c.host)
		if id != c.id || port != c.port || ok != c.ok {
			t.Errorf("%+v Parse(%q) = %q,%q,%v; want %q,%q,%v", c.s, c.host, id, port, ok, c.id, c.port, c.ok)
		}
		if got := c.s.HostRegexp().MatchString(c.host); got != c.ok {
			t.Errorf("%+v HostRegexp(%q) = %v; want %v", c.s, c.host, got, c.ok)
		}
	}
}

func TestParseStyleAndTag(t *testing.T) {
	for in, want := range map[string]string{"": StyleNested, "nested": StyleNested, " Flat ": StyleFlat} {
		if got, err := ParseStyle(in); err != nil || got != want {
			t.Errorf("ParseStyle(%q) = %q,%v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseStyle("wide"); err == nil {
		t.Error("ParseStyle(wide): want error")
	}
	for tag, want := range map[string]bool{
		"": true, "a": true, "eu1": true, "a-b": true, "0123456789012345678901234567890": true,
		"-a": false, "EU": false, "a_b": false, "a.b": false, "01234567890123456789012345678901": false,
	} {
		if got := ValidTag(tag); got != want {
			t.Errorf("ValidTag(%q) = %v; want %v", tag, got, want)
		}
	}
	if s, err := New("ex.com", "nested", "eu1"); err != nil || s.Tag != "" {
		t.Errorf("New nested with tag: %+v %v; tag must be dropped", s, err)
	}
	if _, err := New("ex.com", "flat", "Bad"); err == nil {
		t.Error("New flat with bad tag: want error")
	}
	if s, err := New("ex.com", "flat", "eu1"); err != nil || s.Preview("A", 1) != "s-A-1--eu1.ex.com" {
		t.Errorf("New flat eu1: %+v %v", s, err)
	}
}
