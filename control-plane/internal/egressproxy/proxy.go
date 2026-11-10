// Package egressproxy is the sandbox egress gateway (PLAN §8 #5). Sandboxes
// sit on a network with no route out (docker-compose: internal network); the
// only way out is this HTTP/CONNECT proxy, which:
//
//   - identifies the sandbox by its source IP (never by anything the sandbox
//     says about itself),
//   - applies that sandbox's egress rules: mode "none" allows nothing,
//     "allowlist" only listed hosts (deny-by-default: an empty list allows
//     nothing), "open" everything; the deny list (hosts, IPs, CIDRs) wins in
//     every mode, also against the addresses an allowed name resolves to,
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

// Lookup maps a source IP to the sandbox and its egress rules.
type Lookup func(ctx context.Context, ip string) (sandboxID string, rules policy.EgressRules, ok bool)

// Decision describes one request, for logging and the live network view.
type Decision struct {
	SandboxID string
	Method    string
	Host      string
	Port      string
	IP        string // the address dialled, for allowed requests
	Allowed   bool
	Reason    string
}

// Proxy is an http.Handler implementing a forward proxy.
type Proxy struct {
	Lookup   Lookup
	Log      *slog.Logger
	Resolver *net.Resolver // nil = net.DefaultResolver
	// OnDecision is called for every request (audit / metrics). Optional.
	OnDecision func(Decision)
	// OnBytes is called when an allowed connection closes. Optional.
	OnBytes func(sandboxID, host string, up, down int64)
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
	d := Decision{Method: r.Method}
	sandboxID, rules, ok := p.Lookup(r.Context(), src)
	if !ok {
		p.refuse(w, d, "source is not a running sandbox")
		return
	}
	d.SandboxID = sandboxID
	host, port, err := targetOf(r)
	if err != nil {
		p.refuse(w, d, err.Error())
		return
	}
	d.Host, d.Port = host, port
	if allowed, reason := rules.Check(host); !allowed {
		p.refuse(w, d, reason)
		return
	}
	conn, err := p.dial(r.Context(), host, port, rules)
	if err != nil {
		p.refuse(w, d, err.Error())
		return
	}
	if ap, err := netip.ParseAddrPort(conn.RemoteAddr().String()); err == nil {
		d.IP = ap.Addr().Unmap().String()
	}
	d.Allowed = true
	p.decided(d)
	var up, down int64
	if r.Method == http.MethodConnect {
		up, down = p.tunnel(w, conn)
	} else {
		up, down = p.forward(w, r, conn)
	}
	if p.OnBytes != nil {
		p.OnBytes(sandboxID, host, up, down)
	}
}

func (p *Proxy) decided(d Decision) {
	if p.OnDecision != nil {
		p.OnDecision(d)
	}
}

func (p *Proxy) refuse(w http.ResponseWriter, d Decision, reason string) {
	d.Allowed, d.Reason = false, reason
	p.decided(d)
	p.log().Info("egress denied", "sandbox_id", d.SandboxID, "host", d.Host, "reason", reason)
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

func (p *Proxy) dial(ctx context.Context, host, port string, rules policy.EgressRules) (net.Conn, error) {
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
	denied := false
	for _, a := range addrs {
		never, private := Blocked(a)
		if never {
			continue
		}
		if rules.AddrDenied(a) {
			denied = true
			continue
		}
		if private && !rules.AddrAllowedExplicitly(a) {
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
		if denied {
			return nil, fmt.Errorf("%s resolves only to addresses on this sandbox's block list", host)
		}
		return nil, fmt.Errorf("%s resolves only to blocked addresses", host)
	}
	return nil, lastErr
}

// tunnel relays a CONNECT stream and returns the bytes sent up and down.
func (p *Proxy) tunnel(w http.ResponseWriter, upstream net.Conn) (up, down int64) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return 0, 0
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return 0, 0
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	upc := make(chan int64, 1)
	go func() {
		var n int64
		if buf.Reader.Buffered() > 0 {
			n, _ = io.CopyN(upstream, buf, int64(buf.Reader.Buffered()))
		}
		m, _ := io.Copy(upstream, client)
		_ = upstream.Close()
		upc <- n + m
	}()
	down, _ = io.Copy(client, upstream)
	_ = client.Close()
	return <-upc, down
}

// forward relays one plain-HTTP request over an already-checked connection
// and returns the request and response body sizes.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, upstream net.Conn) (up, down int64) {
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
	cb := &countingBody{rc: out.Body}
	if out.Body != nil && out.Body != http.NoBody {
		out.Body = cb
	}
	resp, err := tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return cb.n, 0
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	down, _ = io.Copy(w, resp.Body)
	return cb.n, down
}

type countingBody struct {
	rc io.ReadCloser
	n  int64
}

func (c *countingBody) Read(b []byte) (int, error) {
	n, err := c.rc.Read(b)
	c.n += int64(n)
	return n, err
}

func (c *countingBody) Close() error { return c.rc.Close() }
