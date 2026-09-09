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

// ReadMessage reads the next data message into a pooled buffer and returns an
// exact-size copy the caller owns, where the library's ReadMessage grows a
// fresh buffer per message. On error p is nil.
func (conn *Conn) ReadMessage() (messageType int, p []byte, err error) {
	messageType, r, err := conn.Conn.NextReader()
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
				grown := make([]byte, len(buf), 2*cap(buf))
				copy(grown, buf)
				buf = append(grown, fr.probe[0])
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
	fr.buf = buf
	return utils.CopyBytes(buf), nil
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
