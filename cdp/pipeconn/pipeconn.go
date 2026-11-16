// Package pipeconn carries DevTools messages over a pair of pipes, the way
// a browser started with --remote-debugging-pipe talks: each message is
// JSON followed by a NUL byte. Nothing listens on a port, so no other
// program on the computer can attach to the browser.
package pipeconn

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"sync"
)

// MaxMessage is the largest message read (screenshots and big trees are a
// few megabytes; this leaves room).
const MaxMessage = 256 << 20

// Conn reads messages from r and writes them to w.
type Conn struct {
	r      *bufio.Reader
	w      io.Writer
	closer func() error
	once   sync.Once
}

// New makes a Conn. close, if not nil, is called once by Close (close the
// pipes and wait for the process there).
func New(r io.Reader, w io.Writer, close func() error) *Conn {
	return &Conn{r: bufio.NewReaderSize(r, 1<<16), w: w, closer: close}
}

// ReadMessage returns the next message without its NUL.
func (c *Conn) ReadMessage() ([]byte, error) {
	var buf bytes.Buffer
	for {
		chunk, err := c.r.ReadSlice(0)
		buf.Write(chunk)
		if buf.Len() > MaxMessage {
			return nil, errors.New("pipeconn: message too large")
		}
		switch err {
		case nil:
			b := buf.Bytes()
			return b[:len(b)-1], nil
		case bufio.ErrBufferFull:
			continue
		default:
			return nil, err
		}
	}
}

// WriteMessage writes msg and its NUL in one write.
func (c *Conn) WriteMessage(msg []byte) error {
	if bytes.IndexByte(msg, 0) >= 0 {
		return errors.New("pipeconn: message contains NUL")
	}
	b := make([]byte, len(msg)+1)
	copy(b, msg)
	_, err := c.w.Write(b)
	return err
}

// Close calls the close function once.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() {
		if c.closer != nil {
			err = c.closer()
		}
	})
	return err
}
