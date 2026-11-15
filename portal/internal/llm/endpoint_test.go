package llm

import "testing"

func TestEndpointKeepsQuery(t *testing.T) {
	cases := map[string]string{
		"https://h/v1":                      "https://h/v1/chat/completions",
		"https://h/v1/":                     "https://h/v1/chat/completions",
		"https://h/v1?XTransformPort=3003":  "https://h/v1/chat/completions?XTransformPort=3003",
		"https://h/v1/?XTransformPort=3003": "https://h/v1/chat/completions?XTransformPort=3003",
		"http://127.0.0.1:3003/v1":          "http://127.0.0.1:3003/v1/chat/completions",
	}
	for in, want := range cases {
		if got := endpoint(in, "/chat/completions"); got != want {
			t.Errorf("endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
