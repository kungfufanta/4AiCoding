package httpapi

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
)

func TestUpstreamMessage(t *testing.T) {
	cases := map[error]string{
		fmt.Errorf("%w: status 401", llm.ErrUpstream):            "HTTP 401 (API key",
		fmt.Errorf("%w: status 404", llm.ErrUpstream):            "HTTP 404 (sai base URL)",
		fmt.Errorf("%w: dial tcp: i/o timeout", llm.ErrUpstream): "cannot reach the provider",
		errors.New("x"): "cannot reach the provider",
	}
	for err, want := range cases {
		if got := upstreamMessage(err); !strings.Contains(got, want) {
			t.Errorf("upstreamMessage(%v) = %q, want it to contain %q", err, got, want)
		}
	}
}
