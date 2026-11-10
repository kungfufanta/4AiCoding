package kube

// Minimal RFC 6455 WebSocket CLIENT, just enough for the Kubernetes
// exec subprotocol (v4.channel.k8s.io):
//
//   - one HTTP Upgrade handshake (token auth),
//   - server->client frames are UNMASKED binary frames whose first byte
//     is the channel (0 stdin, 1 stdout, 2 stderr, 3 error, 4 resize),
//   - client->server frames MUST be masked; we only ever send a pong
//     (for the apiserver's pings) and a close.
//
// The apiserver speaks HTTP/1.1 for upgrades, which is why a plain
// socket + handshake beats dragging in a websocket dependency for one
// call site.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// wsConn is a connected (and handshaken) WebSocket.
type wsConn struct {
	conn      net.Conn
	br        *bufio.Reader
	wchans    wsChannels
	stopWatch func() bool // releases the ctx watcher (nil-safe via AfterFunc)
}

// wsChannels maps the v4.channel.k8s.io byte channels.
type wsChannels struct {
	Stdin, Stdout, Stderr, Error, Resize byte
}

var v4Channels = wsChannels{Stdin: 0, Stdout: 1, Stderr: 2, Error: 3, Resize: 4}

// wsDial performs the upgrade handshake against the api server and
// returns the open connection.
func (c *Client) wsDial(ctx context.Context, path string) (*wsConn, error) {
	u, err := url.Parse(c.base.String() + path)
	if err != nil {
		return nil, err
	}
	hostPort := u.Host
	if !strings.Contains(hostPort, ":") {
		if u.Scheme == "https" {
			hostPort += ":443"
		} else {
			hostPort += ":80"
		}
	}
	var conn net.Conn
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if u.Scheme == "https" {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}
		if tr, ok := c.http.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
			tlsCfg = tr.TLSClientConfig.Clone()
			tlsCfg.ServerName = u.Hostname()
		}
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", hostPort)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", hostPort)
	}
	if err != nil {
		return nil, fmt.Errorf("kube exec: dial: %w", err)
	}
	// Bind the whole exchange to ctx: the handshake gets a bounded
	// deadline; after the upgrade, a ctx cancellation unblocks any
	// pending read (exec streams and log follows both end with it).
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		conn.Close()
		return nil, err
	}
	req := strings.Join([]string{
		"GET " + u.RequestURI() + " HTTP/1.1",
		"Host: " + u.Host,
		"Upgrade: websocket",
		"Connection: Upgrade",
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key),
		"Sec-WebSocket-Version: 13",
		"Sec-WebSocket-Protocol: v4.channel.k8s.io",
		"Authorization: Bearer " + c.tok,
		"\r\n",
	}, "\r\n")
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("kube exec: handshake write: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("kube exec: handshake read: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// The apiserver answers exec errors (missing pod, forbidden) as a
		// normal JSON Status before any upgrade.
		conn.Close()
		se := &StatusError{Code: resp.StatusCode}
		var st struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
		}
		if err := jsonDecodeBody(resp.Body, &st); err == nil {
			se.Message, se.Reason = st.Message, st.Reason
		}
		return nil, se
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") &&
		resp.Header.Get("Sec-WebSocket-Protocol") != "v4.channel.k8s.io" {
		conn.Close()
		return nil, errors.New("kube exec: server did not upgrade to websocket")
	}
	w := &wsConn{conn: conn, br: br, wchans: v4Channels}
	w.stopWatch = context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	return w, nil
}

// execChannel is one decoded v4 channel frame.
type execFrame struct {
	Channel byte
	Payload []byte
}

// readFrame reads one server frame. Returns io.EOF (or a close error)
// when the server ends the stream.
func (w *wsConn) readFrame() (execFrame, error) {
	for {
		h1, err := w.br.ReadByte()
		if err != nil {
			return execFrame{}, err
		}
		h2, err := w.br.ReadByte()
		if err != nil {
			return execFrame{}, err
		}
		fin := h1&0x80 != 0
		opcode := h1 & 0x0f
		masked := h2&0x80 != 0
		l := uint64(h2 & 0x7f)
		switch l {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return execFrame{}, err
			}
			l = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return execFrame{}, err
			}
			l = binary.BigEndian.Uint64(b[:])
		}
		if l > 16<<20 { // sanity bound
			return execFrame{}, errors.New("kube exec: websocket frame too large")
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(w.br, mask[:]); err != nil {
				return execFrame{}, err
			}
		}
		payload := make([]byte, l)
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return execFrame{}, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case 0x2, 0x1: // binary / text — v4 exec frames are binary
			return execFrame{Channel: payload[0], Payload: payload[1:]}, nil
		case 0x9: // ping -> pong (masked, from the client)
			if err := w.writeFrame(0xA, payload); err != nil {
				return execFrame{}, err
			}
			// continue reading
		case 0x8: // close
			if !fin {
				continue
			}
			return execFrame{}, io.EOF
		default:
			// ignore unknown control frames
		}
	}
}

// writeFrame writes one masked client frame (opcode 0x2 binary).
func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x80|opcode)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(0x80|n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(n))
		hdr = append(hdr, b[:]...)
	default:
		hdr = append(hdr, 0x80|127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, b[:]...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.conn.Write(append(hdr, masked...)); err != nil {
		return err
	}
	return nil
}

func (w *wsConn) close() {
	if w.stopWatch != nil {
		w.stopWatch()
	}
	// Best-effort close frame, then drop the TCP connection.
	_ = w.conn.SetDeadline(time.Now().Add(2 * time.Second))
	_ = w.writeFrame(0x8, nil)
	_ = w.conn.Close()
}
