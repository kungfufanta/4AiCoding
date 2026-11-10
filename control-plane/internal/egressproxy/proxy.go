// Package egressproxy is the sandbox egress gateway (PLAN §8 #5). Sandboxes
// sit on a network with no route out (docker-compose: internal network); the
// only way out is this HTTP/CONNECT proxy, which:
//
//   - identifies the sandbox by its source IP (never by anything the sandbox
//     says about itself),
//   - allows a destination only if its host is on that sandbox's egress
//     allowlist (deny-by-default: an empty list allows nothing),
//   - resolves names itself and refuses loopback, link-local (including the
//     cloud metadata endpoint 169.254.169.254), multicast and unspecified
//     addresses always, and private (RFC 1918 / ULA) addresses unless the
//     allowlist names that exact IP — so an allowed name cannot be pointed at
//     the internal network (DNS rebinding).
package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/policy"
)

// Lookup maps a source IP to the sandbox and its egress allowlist.
type Lookup func(ctx context.Context, ip string) (sandboxID string, allow []string, ok bool)

// Proxy is an http.Handler implementing a forward proxy.
type Proxy struct {
	Lookup   Lookup
	Log      *slog.Logger
	Resolver *net.Resolver // nil = net.DefaultResolver
	// OnDecision is called for every request (audit / metrics). Optional.
	OnDecision func(sandboxID, host string, allowed bool, reason string)
	// DialTimeout bounds upstream connects (default 10s).
	DialTimeout time.Duration
}

// ErrBlocked is returned when a destination address is never allowed.
var ErrBlocked = errors.New("destination address is blocked")

func (p *Proxy) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	src, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		src = r.RemoteAddr
	}
	sandboxID, allow, ok := p.Lookup(r.Context(), src)
	if !ok {
		p.refuse(w, "", "", "source is not a running sandbox")
		return
	}
	host, port, err := targetOf(r)
	if err != nil {
		p.refuse(w, sandboxID, "", err.Error())
		return
	}
	if !policy.HostAllowed(allow, host) {
		p.refuse(w, sandboxID, host, "host is not on this sandbox's egress allowlist")
		return
	}
	conn, err := p.dial(r.Context(), host, port, allow)
	if err != nil {
		p.refuse(w, sandboxID, host, err.Error())
		return
	}
	p.decided(sandboxID, host, true, "")
	if r.Method == http.MethodConnect {
		p.tunnel(w, conn)
		return
	}
	p.forward(w, r, conn)
}

func (p *Proxy) decided(sandboxID, host string, allowed bool, reason string) {
	if p.OnDecision != nil {
		p.OnDecision(sandboxID, host, allowed, reason)
	}
}

func (p *Proxy) refuse(w http.ResponseWriter, sandboxID, host, reason string) {
	p.decided(sandboxID, host, false, reason)
	p.log().Info("egress denied", "sandbox_id", sandboxID, "host", host, "reason", reason)
	http.Error(w, "egress denied: "+reason, http.StatusForbidden)
}

// targetOf returns the destination host and port of a proxy request.
func targetOf(r *http.Request) (string, string, error) {
	if r.Method == http.MethodConnect {
		h, port, err := net.SplitHostPort(r.Host)
		if err != nil {
			return "", "", fmt.Errorf("bad CONNECT target")
		}
		return strings.ToLower(h), port, nil
	}
	if r.URL == nil || !r.URL.IsAbs() || r.URL.Scheme != "http" {
		return "", "", fmt.Errorf("only absolute http:// URLs or CONNECT are proxied")
	}
	h := r.URL.Hostname()
	port := r.URL.Port()
	if port == "" {
		port = "80"
	}
	return strings.ToLower(h), port, nil
}

// Blocked reports whether an address is never a valid egress destination,
// and whether it is private (allowed only when named explicitly).
func Blocked(a netip.Addr) (never bool, private bool) {
	a = a.Unmap()
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true, false
	}
	// 100.64.0.0/10 (CGNAT) and 0.0.0.0/8 are not routable destinations.
	if a.Is4() {
		b := a.As4()
		if b[0] == 0 || (b[0] == 100 && b[1]&0xc0 == 64) {
			return true, false
		}
	}
	return false, a.IsPrivate()
}

func (p *Proxy) dial(ctx context.Context, host, port string, allow []string) (net.Conn, error) {
	if _, err := strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("bad port")
	}
	res := p.Resolver
	if res == nil {
		res = net.DefaultResolver
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		ips, err := res.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %v", host, err)
		}
		addrs = ips
	}
	timeout := p.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	var lastErr error = ErrBlocked
	for _, a := range addrs {
		never, private := Blocked(a)
		if never {
			continue
		}
		if private && !explicitlyAllowed(allow, a) {
			continue
		}
		d := net.Dialer{Timeout: timeout}
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	if errors.Is(lastErr, ErrBlocked) {
		return nil, fmt.Errorf("%s resolves only to blocked addresses", host)
	}
	return nil, lastErr
}

func explicitlyAllowed(allow []string, a netip.Addr) bool {
	s := a.Unmap().String()
	for _, x := range allow {
		if x == s {
			return true
		}
	}
	return false
}

func (p *Proxy) tunnel(w http.ResponseWriter, upstream net.Conn) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		if buf.Reader.Buffered() > 0 {
			_, _ = io.CopyN(upstream, buf, int64(buf.Reader.Buffered()))
		}
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
}

// forward relays one plain-HTTP request over an already-checked connection.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, upstream net.Conn) {
	defer upstream.Close()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	tr := &http.Transport{
		DialContext:       func(context.Context, string, string) (net.Conn, error) { return upstream, nil },
		DisableKeepAlives: true,
	}
	resp, err := tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
