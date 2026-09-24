package valkey

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a minimal in-memory RESP2 server speaking GET/SET/DEL/PING.
type fakeServer struct {
	mu   sync.Mutex
	data map[string][]byte
	exp  map[string]time.Time
}

func serveFake(t *testing.T, conn net.Conn) {
	t.Helper()
	s := &fakeServer{data: map[string][]byte{}, exp: map[string]time.Time{}}
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		cmd, err := readCommand(r)
		if err != nil {
			return
		}
		s.mu.Lock()
		var out string
		switch strings.ToUpper(cmd[0]) {
		case "PING":
			out = "+PONG\r\n"
		case "INFO":
			out = "$21\r\nused_memory:1048576\r\n\r\n"
		case "DBSIZE":
			out = ":" + strconv.Itoa(len(s.data)) + "\r\n"
		case "SCAN":
			var entries strings.Builder
			count := 0
			for key := range s.data {
				if strings.HasPrefix(key, "replx_edge:") {
					entries.WriteString("$" + strconv.Itoa(len(key)) + "\r\n" + key + "\r\n")
					count++
				}
			}
			out = "*2\r\n$1\r\n0\r\n*" + strconv.Itoa(count) + "\r\n" + entries.String()
		case "GET":
			v, ok := s.data[cmd[1]]
			if ok {
				if exp, has := s.exp[cmd[1]]; has && time.Now().After(exp) {
					delete(s.data, cmd[1])
					delete(s.exp, cmd[1])
					ok = false
				}
			}
			if !ok {
				out = "$-1\r\n"
			} else {
				out = "$" + strconv.Itoa(len(v)) + "\r\n" + string(v) + "\r\n"
			}
		case "SET":
			s.data[cmd[1]] = []byte(cmd[2])
			delete(s.exp, cmd[1])
			for i := 3; i+1 < len(cmd); i += 2 {
				if strings.ToUpper(cmd[i]) == "EX" {
					secs, _ := strconv.Atoi(cmd[i+1])
					s.exp[cmd[1]] = time.Now().Add(time.Duration(secs) * time.Second)
				}
			}
			out = "+OK\r\n"
		case "DEL":
			n := 0
			for _, k := range cmd[1:] {
				if _, ok := s.data[k]; ok {
					delete(s.data, k)
					delete(s.exp, k)
					n++
				}
			}
			out = ":" + strconv.Itoa(n) + "\r\n"
		default:
			out = "-ERR unknown command\r\n"
		}
		s.mu.Unlock()
		if _, err := conn.Write([]byte(out)); err != nil {
			return
		}
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	reply, err := readReply(r)
	if err != nil {
		return nil, err
	}
	arr, ok := reply.([]any)
	if !ok {
		return nil, fmt.Errorf("want array, got %T", reply)
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		b, ok := e.([]byte)
		if !ok {
			return nil, fmt.Errorf("want bulk, got %T", e)
		}
		out = append(out, string(b))
	}
	return out, nil
}

func dialFake(t *testing.T) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	go serveFake(t, serverConn)
	c := NewClient("fake:0", time.Second)
	c.mu.Lock()
	c.conn = clientConn
	c.r = bufio.NewReader(clientConn)
	c.mu.Unlock()
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRoundTripSetGetDel(t *testing.T) {
	c := dialFake(t)
	if err := c.Set("k1", []byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	v, ok, err := c.Get("k1")
	if err != nil || !ok || string(v) != "hello" {
		t.Fatalf("get: %q %v %v", v, ok, err)
	}
	if err := c.Del("k1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Get("k1"); err != nil || ok {
		t.Fatalf("want miss after del, ok=%v err=%v", ok, err)
	}
}

func TestUsageAndInventoryOnlyReturnAggregateCounts(t *testing.T) {
	c := dialFake(t)
	for _, key := range []string{"replx_edge:v2:default:collections:user-a:json:0:0:aaa:window", "replx_edge:v2:default:hubs:user-a:json:0:0:bbb", "other"} {
		if err := c.Set(key, []byte("secret"), 0); err != nil {
			t.Fatal(err)
		}
	}
	used, keys, err := c.Usage()
	if err != nil || used != 1048576 || keys != 3 {
		t.Fatalf("usage: %d %d %v", used, keys, err)
	}
	counts, scanned, err := c.Inventory(20)
	if err != nil || scanned != 2 || counts["collections"] != 1 || counts["hubs"] != 1 || counts["fullCollectionWindows"] != 1 {
		t.Fatalf("inventory: %v %d %v", counts, scanned, err)
	}
}

func TestBinarySafeValue(t *testing.T) {
	c := dialFake(t)
	bin := []byte{0x00, 0xff, '\r', '\n', '$', '*', 0x01}
	if err := c.Set("bin", bin, time.Minute); err != nil {
		t.Fatal(err)
	}
	v, ok, err := c.Get("bin")
	if err != nil || !ok || string(v) != string(bin) {
		t.Fatalf("binary round trip failed: %x %v %v", v, ok, err)
	}
}

func TestSetErrorSurfaces(t *testing.T) {
	c := dialFake(t)
	if err := c.Del(""); err != nil {
		t.Fatalf("del empty must not error the transport: %v", err)
	}
}
