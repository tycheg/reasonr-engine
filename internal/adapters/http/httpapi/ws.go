package httpapi

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// WebSocket (RFC 6455) without dependencies: the server only sends
// notifications as text frames, answers the client's ping and close and
// pings the connection itself. Client data is not needed and is dropped.

const (
	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	opText       = 0x1
	opClose      = 0x8
	opPing       = 0x9
	opPong       = 0xA
	wsMaxPayload = 64 << 10 // the largest client frame
	writeTimeout = 10 * time.Second
)

// Close codes.
const (
	closeNormal    = 1000
	closeGoingAway = 1001
	closeProtocol  = 1002
	closeTooBig    = 1009
	closeInternal  = 1011
)

// websocket is GET /api/ws: an actor's notifications from other actors.
func (s *Server) websocket(w http.ResponseWriter, r *http.Request, c *call) {
	if !headerHas(r.Header, "Connection", "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "a WebSocket is needed (Upgrade: websocket)", http.StatusUpgradeRequired)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Header.Get("Sec-WebSocket-Version") != "13" || len(key) != 24 {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "WebSocket version 13 is needed", http.StatusBadRequest)
		return
	}
	a := reasonr.NewArena()
	tenant, caller, sc, ok := s.authenticate(w, r, c, a, true)
	if !ok {
		return
	}
	if caller.Subject.Kind == reasonr.KindNone {
		http.Error(w, "only actors get notifications", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	sub, err := s.x.Subscribe(ctx, tenant, a, caller.Subject)
	if err != nil {
		s.fail(w, r, c, err)
		return
	}
	defer sub.Close()
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		s.fail(w, r, c, err)
		return
	}
	c.status = http.StatusSwitchingProtocols
	defer conn.Close()
	accept := sha1.Sum([]byte(key + wsGUID))
	proto := ""
	if headerHas(r.Header, "Sec-WebSocket-Protocol", WSProtocol) {
		proto = "Sec-WebSocket-Protocol: " + WSProtocol + "\r\n" // a browser requires choosing an offered one
	}
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" + proto +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(accept[:]) + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}
	ws := &wsConn{conn: conn, w: rw.Writer}
	control := make(chan frame, 4)
	readErr := make(chan int, 1)
	go func() { readErr <- readFrames(rw.Reader, control) }()
	ping := time.NewTicker(s.opts.PingInterval)
	defer ping.Stop()
	for {
		select {
		case d, ok := <-sub.C():
			if !ok {
				ws.close(closeInternal)
				return
			}
			b, err := s.notification(nil, sc, d.Arena, d.Notification)
			if err != nil {
				s.log.Error("the notification cannot be encoded", "error", err)
				continue
			}
			if ws.write(opText, b) != nil {
				return
			}
		case f := <-control:
			switch f.op {
			case opPing:
				if ws.write(opPong, f.payload) != nil {
					return
				}
			case opClose:
				ws.close(closeNormal)
				return
			}
		case code := <-readErr:
			ws.close(code)
			return
		case <-ping.C:
			if ws.write(opPing, nil) != nil {
				return
			}
		case <-ctx.Done():
			ws.close(closeGoingAway)
			return
		}
	}
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

type wsConn struct {
	conn net.Conn
	w    *bufio.Writer
}

// write sends an unmasked frame (server frames are not masked).
func (c *wsConn) write(op byte, payload []byte) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	var h [10]byte
	h[0] = 0x80 | op
	n := 2
	switch l := len(payload); {
	case l < 126:
		h[1] = byte(l)
	case l <= 0xffff:
		h[1] = 126
		binary.BigEndian.PutUint16(h[2:], uint16(l))
		n = 4
	default:
		h[1] = 127
		binary.BigEndian.PutUint64(h[2:], uint64(l))
		n = 10
	}
	if _, err := c.w.Write(h[:n]); err != nil {
		return err
	}
	if _, err := c.w.Write(payload); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *wsConn) close(code int) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(code))
	_ = c.write(opClose, b[:])
}

type frame struct {
	op      byte
	payload []byte
}

// readFrames reads client frames: it passes ping and close to control and
// drops data. It returns a close code on a protocol error or a disconnect.
func readFrames(r *bufio.Reader, control chan<- frame) int {
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return closeGoingAway
		}
		op, masked := h[0]&0x0f, h[1]&0x80 != 0
		length := uint64(h[1] & 0x7f)
		switch length {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return closeGoingAway
			}
			length = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return closeGoingAway
			}
			length = binary.BigEndian.Uint64(b[:])
		}
		if !masked || h[0]&0x70 != 0 || op >= opClose && (length > 125 || h[0]&0x80 == 0) {
			return closeProtocol // client frames are masked; control ones are short and whole
		}
		if length > wsMaxPayload {
			return closeTooBig
		}
		var mask [4]byte
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return closeGoingAway
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return closeGoingAway
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		if op == opPing || op == opClose {
			select {
			case control <- frame{op: op, payload: payload}:
			default: // the writer cannot keep up: an extra ping is not needed
			}
			if op == opClose {
				return closeNormal
			}
		}
	}
}

// errNotWebSocket: the server's response did not switch the protocol (for a client).
var errNotWebSocket = errors.New("httpapi: the server did not switch to WebSocket")

// DialWebSocket is a minimal WebSocket client for tests and tools: it
// connects to url (ws://…) with the headers h and returns a function that
// reads text frames, and a close function.
func DialWebSocket(ctx context.Context, url string, h http.Header) (read func() ([]byte, error), closeFn func() error, err error) {
	addr, path, ok := strings.Cut(strings.TrimPrefix(url, "ws://"), "/")
	if !ok {
		path = ""
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	key := base64.StdEncoding.EncodeToString([]byte("reasonr-ws-key!!"))
	req := fmt.Sprintf("GET /%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n",
		path, addr, key)
	for k, vs := range h {
		for _, v := range vs {
			req += k + ": " + v + "\r\n"
		}
	}
	if _, err := conn.Write([]byte(req + "\r\n")); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, nil, fmt.Errorf("%w: %s", errNotWebSocket, resp.Status)
	}
	read = func() ([]byte, error) {
		for {
			var h [2]byte
			if _, err := io.ReadFull(br, h[:]); err != nil {
				return nil, err
			}
			length := uint64(h[1] & 0x7f)
			switch length {
			case 126:
				var b [2]byte
				if _, err := io.ReadFull(br, b[:]); err != nil {
					return nil, err
				}
				length = uint64(binary.BigEndian.Uint16(b[:]))
			case 127:
				var b [8]byte
				if _, err := io.ReadFull(br, b[:]); err != nil {
					return nil, err
				}
				length = binary.BigEndian.Uint64(b[:])
			}
			payload := make([]byte, length)
			if _, err := io.ReadFull(br, payload); err != nil {
				return nil, err
			}
			switch h[0] & 0x0f {
			case opText:
				return payload, nil
			case opClose:
				return nil, io.EOF
			}
		}
	}
	closeFn = func() error {
		// A client close frame with mask 0: the payload does not change.
		_, _ = conn.Write([]byte{0x80 | opClose, 0x80 | 2, 0, 0, 0, 0, 0x03, 0xe8})
		return conn.Close()
	}
	return read, closeFn, nil
}
