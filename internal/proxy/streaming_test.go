package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/gateway"
)

func TestSSEStreamsWithoutBuffering(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/:/eventsource/notifications" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: event-%d\n\n", i)
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel"})
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := httptest.NewServer(h)
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/:/eventsource/notifications")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("sse: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(string(body), fmt.Sprintf("event-%d", i)) {
			t.Fatalf("missing event %d: %q", i, body)
		}
	}
}

func TestWebSocketTunnel(t *testing.T) {
	// Fake origin: answers the handshake, then echoes raw bytes.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		hj := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_, _ = io.Copy(conn, conn)
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel"})
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := httptest.NewServer(h)
	defer proxySrv.Close()

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxySrv.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /:/websocket/notifications HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("handshake: %q %v", status, err)
	}
	// Drain headers, then verify echo through the tunnel.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := conn.Write([]byte("ping-tunnel")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len("ping-tunnel"))
	if _, err := io.ReadFull(br, echo); err != nil || string(echo) != "ping-tunnel" {
		t.Fatalf("echo: %q %v", echo, err)
	}
}

func TestStreamingPathsAreControl(t *testing.T) {
	for _, p := range []string{"/:/eventsource/notifications", "/:/websocket/notifications"} {
		if gateway.IsBulkMediaRoute(p) {
			t.Fatalf("%s must be control, not media", p)
		}
	}
}
