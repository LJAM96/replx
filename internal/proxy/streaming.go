package proxy

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Event paths for Plex real-time notifications. Both are CONTROL traffic:
// server-sent events stream text, WebSocket tunnels raw bytes. Neither
// carries bulk video, and neither may be subjected to the 60s media-client
// timeout or hop-by-hop header stripping.
const (
	ssePath = "/:/eventsource/notifications"
	wsPath  = "/:/websocket/notifications"
)

func isSSE(r *http.Request) bool {
	return r.URL.Path == ssePath
}

func isWebSocket(r *http.Request) bool {
	if r.URL.Path != wsPath {
		return false
	}
	for _, v := range strings.Split(r.Header.Get("Upgrade"), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "websocket") {
			return true
		}
	}
	return false
}

// serveStreaming dispatches SSE and WebSocket requests. It reports whether
// it handled the request.
func (h *Handler) serveStreaming(w http.ResponseWriter, r *http.Request, id string, start time.Time) bool {
	switch {
	case isWebSocket(r):
		h.tunnelWebSocket(w, r, id, start)
		return true
	case isSSE(r):
		h.streamSSE(w, r, id, start)
		return true
	default:
		return false
	}
}

func (h *Handler) streamSSE(w http.ResponseWriter, r *http.Request, id string, start time.Time) {
	target := *h.origin
	target.Path = singleJoin(h.origin.Path, r.URL.Path)
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
	if err != nil {
		h.writeBadGateway(w, r, id, "control", start)
		return
	}
	copyHeaders(out.Header, r.Header)
	out.Header.Set(RequestIDHeader, id)
	out.Host = h.origin.Host
	// No timeout: lifetime is bound to the client request context.
	resp, err := http.DefaultClient.Do(out) //nolint:gosec // admin-configured origin only
	if err != nil {
		h.writeBadGateway(w, r, id, "control", start)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set(RequestIDHeader, id)
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusBadGateway)
		h.emit(r, id, "control", http.StatusBadGateway, start, map[string]any{"decision": "no-flusher"})
		return
	}
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			flusher.Flush()
		}
		if err != nil {
			break
		}
	}
	h.emit(r, id, "control", resp.StatusCode, start, map[string]any{"decision": "streamed"})
}

// tunnelWebSocket hijacks the client connection and pipes raw bytes to the
// origin, preserving the WebSocket handshake end to end.
func (h *Handler) tunnelWebSocket(w http.ResponseWriter, r *http.Request, id string, start time.Time) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		h.writeBadGateway(w, r, id, "control", start)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		h.emit(r, id, "control", http.StatusBadGateway, start, map[string]any{"decision": "hijack-failed"})
		return
	}
	defer clientConn.Close()

	originConn, err := h.dialOrigin()
	if err != nil {
		_, _ = fmt.Fprint(clientConn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		h.emit(r, id, "control", http.StatusBadGateway, start, map[string]any{"decision": "origin-dial-failed"})
		return
	}
	defer originConn.Close()

	if err := r.Write(originConn); err != nil {
		h.emit(r, id, "control", http.StatusBadGateway, start, map[string]any{"decision": "origin-write-failed"})
		return
	}
	h.emit(r, id, "control", http.StatusSwitchingProtocols, start, map[string]any{"decision": "tunneled"})
	go func() { _, _ = io.Copy(originConn, clientConn) }()
	_, _ = io.Copy(clientConn, originConn)
}

func (h *Handler) dialOrigin() (net.Conn, error) {
	host := h.origin.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		if h.origin.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if h.origin.Scheme == "https" {
		name := host
		if h, _, err := net.SplitHostPort(host); err == nil {
			name = h
		}
		return tls.DialWithDialer(dialer, "tcp", host, &tls.Config{
			ServerName: name,
			MinVersion: tls.VersionTLS12,
		})
	}
	return dialer.Dial("tcp", host)
}
