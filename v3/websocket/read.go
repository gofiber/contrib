package websocket

import (
	"errors"
	"io"
	"sync"

	"github.com/gofiber/utils/v2"
)

const (
	frameBufferInitial  = 512
	frameBufferRetained = 64 << 10
)

var framePool = sync.Pool{New: func() any { return new(frameReader) }}

type frameReader struct {
	buf   []byte
	probe [1]byte
}

// ReadMessage reads the next data message and returns a buffer the caller
// owns. A message up to frameBufferRetained is read into a pooled buffer and
// returned as an exact-size copy; a larger one is returned in the buffer it
// was read into, grown by at most a quarter past its size, as the library's
// ReadMessage grows its own. Bound message size with SetReadLimit. On error p
// is nil.
func (conn *Conn) ReadMessage() (messageType int, p []byte, err error) {
	messageType, r, err := conn.NextReader()
	if err != nil {
		return messageType, nil, err
	}
	fr := framePool.Get().(*frameReader)
	p, err = fr.readAll(r)
	fr.release()
	return messageType, p, err
}

func (fr *frameReader) readAll(r io.Reader) ([]byte, error) {
	buf := fr.buf[:0]
	if cap(buf) == 0 {
		buf = make([]byte, 0, frameBufferInitial)
	}
	for {
		if len(buf) == cap(buf) {
			// Probe one byte so a message that exactly fills the buffer does not double it.
			n, err := r.Read(fr.probe[:])
			if n > 0 {
				buf = append(grow(buf), fr.probe[0])
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fr.buf = buf
				return nil, err
			}
			continue
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fr.buf = buf
			return nil, err
		}
	}
	if cap(buf) > frameBufferRetained {
		// Too big for the pool: the caller takes it as it is, rather than
		// paying for a second copy of a large message.
		fr.buf = nil
		return buf, nil
	}
	fr.buf = buf
	return utils.CopyBytes(buf), nil
}

// grow makes room for at least one more byte: doubling up to the size the
// pool retains, then by a quarter, as append would, so a large message ends
// in a buffer at most 25% bigger than itself.
func grow(buf []byte) []byte {
	c := cap(buf)
	if c < frameBufferRetained {
		c = min(2*c, frameBufferRetained)
	} else {
		c += c / 4
	}
	grown := make([]byte, len(buf), c)
	copy(grown, buf)
	return grown
}

func (fr *frameReader) release() {
	fr.trim()
	framePool.Put(fr)
}

// trim drops a buffer that grew past frameBufferRetained.
func (fr *frameReader) trim() {
	if cap(fr.buf) > frameBufferRetained {
		fr.buf = nil
	}
}
