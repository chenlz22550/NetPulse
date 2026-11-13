// Package telnet implements the Telnet framing used by the built-in terminal.
package telnet

import (
	"bytes"
	"io"
	"sync"
)

const (
	iac  = 255
	dont = 254
	do   = 253
	wont = 252
	will = 251
	sb   = 250
	se   = 240
)

// Client strips negotiations from received data and serializes writes to the wire.
// Read must have a single caller; Send and Resize may run concurrently with it.
type Client struct {
	rw            io.ReadWriter
	mu            sync.Mutex
	state, verb   byte
	local, remote [256]bool
	sub           []byte
	cols, rows    int
}

func New(rw io.ReadWriter) *Client { return &Client{rw: rw, cols: 100, rows: 30} }

func (c *Client) write(b []byte) error {
	for len(b) > 0 {
		n, err := c.rw.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func escape(b []byte) []byte { return bytes.ReplaceAll(b, []byte{iac}, []byte{iac, iac}) }

// Send accepts terminal input; Enter is encoded as Telnet CR LF.
func (c *Client) Send(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\r"))
	b = bytes.ReplaceAll(b, []byte("\r"), []byte("\r\n"))
	return c.write(escape(b))
}

func (c *Client) window() error {
	b := []byte{byte(c.cols >> 8), byte(c.cols), byte(c.rows >> 8), byte(c.rows)}
	p := append([]byte{iac, sb, 31}, escape(b)...)
	return c.write(append(p, iac, se))
}

func (c *Client) Resize(cols, rows int) error {
	if cols < 1 || cols > 1000 || rows < 1 || rows > 500 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cols, c.rows = cols, rows
	if c.local[31] {
		return c.window()
	}
	return nil
}

func (c *Client) negotiate(verb, opt byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch verb {
	case do:
		if opt != 0 && opt != 3 && opt != 24 && opt != 31 {
			return c.write([]byte{iac, wont, opt})
		}
		if c.local[opt] {
			return nil
		}
		c.local[opt] = true
		if err := c.write([]byte{iac, will, opt}); err != nil {
			return err
		}
		if opt == 31 {
			return c.window()
		}
	case dont:
		if c.local[opt] {
			c.local[opt] = false
			return c.write([]byte{iac, wont, opt})
		}
	case will:
		if opt != 0 && opt != 1 && opt != 3 {
			return c.write([]byte{iac, dont, opt})
		}
		if !c.remote[opt] {
			c.remote[opt] = true
			return c.write([]byte{iac, do, opt})
		}
	case wont:
		if c.remote[opt] {
			c.remote[opt] = false
			return c.write([]byte{iac, dont, opt})
		}
	}
	return nil
}

// Read returns only display data, preserving parser state across fragmented reads.
func (c *Client) Read(out []byte) (int, error) {
	b := make([]byte, len(out))
	for {
		n, err := c.rw.Read(b)
		used := 0
		for _, v := range b[:n] {
			switch c.state {
			case 0:
				if v == iac {
					c.state = 1
				} else {
					out[used] = v
					used++
				}
			case 1:
				switch v {
				case iac:
					out[used] = v
					used++
					c.state = 0
				case do, dont, will, wont:
					c.verb = v
					c.state = 2
				case sb:
					c.sub = c.sub[:0]
					c.state = 3
				default:
					c.state = 0
				}
			case 2:
				c.state = 0
				if e := c.negotiate(c.verb, v); e != nil {
					return used, e
				}
			case 3:
				if v == iac {
					c.state = 4
				} else if len(c.sub) < 1024 {
					c.sub = append(c.sub, v)
				}
			case 4:
				if v == se {
					c.state = 0
					c.mu.Lock()
					if len(c.sub) == 2 && c.sub[0] == 24 && c.sub[1] == 1 && c.local[24] {
						err = c.write(append([]byte{iac, sb, 24, 0}, append([]byte("xterm"), iac, se)...))
					}
					c.mu.Unlock()
					if err != nil {
						return used, err
					}
				} else {
					if v == iac && len(c.sub) < 1024 {
						c.sub = append(c.sub, v)
					}
					c.state = 3
				}
			}
		}
		if used > 0 || err != nil {
			return used, err
		}
	}
}
