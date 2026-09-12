// Package valkey checks Valkey reachability with a stdlib RESP PING.
//
// Cache is best-effort in Replx Edge: Valkey down degrades to PMS
// fall-through, never to failed readiness. This package intentionally
// stays dependency-free; the full cache client lands with the Delta
// (user scoped cache) phase.
package valkey

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
)

// Ping dials addr (host:port), issues PING and expects +PONG.
// Any error or unexpected reply reports false with no error.
func Ping(addr string, timeout time.Duration) bool {
	if addr == "" {
		return false
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintf(conn, "PING\r\n"); err != nil {
		return false
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.TrimSpace(line) == "+PONG"
}
