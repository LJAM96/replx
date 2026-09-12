package valkey

import (
	"bufio"
	"net"
	"testing"
	"time"
)

func TestPingPong(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				line, _ := r.ReadString('\n')
				if line == "PING\r\n" {
					_, _ = c.Write([]byte("+PONG\r\n"))
				}
			}(c)
		}
	}()
	if !Ping(ln.Addr().String(), time.Second) {
		t.Fatal("expected PONG")
	}
	if Ping("127.0.0.1:1", 100*time.Millisecond) {
		t.Fatal("expected failure on closed port")
	}
}
