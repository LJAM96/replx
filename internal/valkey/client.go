// Valkey RESP cache client: GET/SET/DEL over a single mutex-held TCP
// connection with reconnect-once. Stdlib only.
//
// Cache is best-effort: every method fails open to the caller, which falls
// through to the PMS origin. A nil *Client is a disabled cache.
package valkey

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is a minimal RESP2 client for string keys and binary-safe values.
type Client struct {
	addr    string
	timeout time.Duration
	mu      sync.Mutex
	conn    net.Conn
	r       *bufio.Reader
}

// NewClient returns a disconnected client dialling addr on first use.
func NewClient(addr string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Client{addr: addr, timeout: timeout}
}

// Close releases the underlying connection, if any.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.r = nil
	return err
}

func (c *Client) ensureLocked() error {
	if c.conn != nil {
		return nil
	}
	if c.addr == "" {
		return fmt.Errorf("valkey: no address")
	}
	conn, err := net.DialTimeout("tcp", c.addr, c.timeout)
	if err != nil {
		return err
	}
	c.conn = conn
	c.r = bufio.NewReader(conn)
	return nil
}

func (c *Client) dropLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.r = nil
}

// roundTrip writes one command and parses one reply, reconnecting once on
// transport failure. Callers hold no locks; serialization is internal.
func (c *Client) roundTrip(args ...string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLocked(); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b.WriteString("$" + strconv.Itoa(len(a)) + "\r\n" + a + "\r\n")
	}
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		c.dropLocked()
		return nil, err
	}
	reply, err := readReply(c.r)
	if err != nil {
		c.dropLocked()
		return nil, err
	}
	if e, ok := reply.(respError); ok {
		return nil, fmt.Errorf("valkey: %s", string(e))
	}
	return reply, nil
}

// Get returns the value and true, or false when the key is absent.
func (c *Client) Get(key string) ([]byte, bool, error) {
	reply, err := c.roundTrip("GET", key)
	if err != nil {
		return nil, false, err
	}
	if reply == nil {
		return nil, false, nil
	}
	b, ok := reply.([]byte)
	if !ok {
		return nil, false, fmt.Errorf("valkey: unexpected GET reply %T", reply)
	}
	return b, true, nil
}

// Set stores val under key with an optional TTL (ttl<=0 means persist).
func (c *Client) Set(key string, val []byte, ttl time.Duration) error {
	var reply any
	var err error
	if ttl > 0 {
		reply, err = c.roundTrip("SET", key, string(val), "EX", strconv.FormatInt(int64(ttl/time.Second), 10))
	} else {
		reply, err = c.roundTrip("SET", key, string(val))
	}
	if err != nil {
		return err
	}
	if s, ok := reply.(string); !ok || s != "OK" {
		return fmt.Errorf("valkey: unexpected SET reply %v", reply)
	}
	return nil
}

// Del removes key. Missing keys are not an error.
func (c *Client) Del(key string) error {
	reply, err := c.roundTrip("DEL", key)
	if err != nil {
		return err
	}
	if _, ok := reply.(int64); !ok {
		return fmt.Errorf("valkey: unexpected DEL reply %v", reply)
	}
	return nil
}

type respError string

// readReply parses one RESP2 reply: +simple, -error, :integer, $bulk
// (nil for $-1), *-arrays of the same (nil for *-1).
func readReply(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 {
		return nil, fmt.Errorf("valkey: short reply %q", line)
	}
	typ, body := line[0], strings.TrimSuffix(line[1:], "\r\n")
	switch typ {
	case '+':
		return body, nil
	case '-':
		return respError(body), nil
	case ':':
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("valkey: bad integer %q", body)
		}
		return n, nil
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, fmt.Errorf("valkey: bad bulk length %q", body)
		}
		if n < 0 {
			return nil, nil // nil bulk
		}
		buf := make([]byte, n+2)
		for i := 0; i < len(buf); {
			m, err := r.Read(buf[i:])
			if err != nil {
				return nil, err
			}
			i += m
		}
		return buf[:n], nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, fmt.Errorf("valkey: bad array length %q", body)
		}
		if n < 0 {
			return nil, nil // nil array
		}
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			e, err := readReply(r)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("valkey: unknown reply type %q", typ)
	}
}
