package websocket

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/gofiber/utils/v2"
)

// readLimitCloseWait bounds the 1009 close frame, as the library bounds its own.
const readLimitCloseWait = time.Second

const (
	frameBufferInitial  = 512
	frameBufferRetained = 64 << 10
)

var framePool = sync.Pool{New: func() any { return new(frameReader) }}

type frameReader struct {
	buf   []byte
	probe [1]byte
}

// SetReadLimit sets the maximum message size. The library counts wire bytes;
// ReadMessage and ReadJSON also count what a compressed message inflates to.
func (conn *Conn) SetReadLimit(limit int64) {
	conn.readLimit = limit
	conn.Conn.SetReadLimit(limit)
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
	p, err = fr.readAll(r, conn.readLimit)
	fr.release()
	if errors.Is(err, ErrReadLimit) {
		// Sticky like the library's: nothing is handed out after the 1009 close.
		conn.readErr = err
		_ = conn.WriteControl(CloseMessage, FormatCloseMessage(CloseMessageTooBig, ""), time.Now().Add(readLimitCloseWait))
		return messageType, nil, err
	}
	return messageType, p, err
}

// NextReader is the library's NextReader; it also returns the ErrReadLimit
// ReadMessage hit, keeping read errors permanent across both.
func (conn *Conn) NextReader() (messageType int, r io.Reader, err error) {
	if conn.readErr != nil {
		return -1, nil, conn.readErr // the library's noFrame
	}
	return conn.Conn.NextReader()
}

// ReadJSON is the library's ReadJSON, read through ReadMessage so SetReadLimit
// also bounds an inflated message.
func (conn *Conn) ReadJSON(v any) error {
	_, p, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	err = json.NewDecoder(bytes.NewReader(p)).Decode(v)
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// readAll reads r to EOF; a positive limit stops it with ErrReadLimit.
func (fr *frameReader) readAll(r io.Reader, limit int64) ([]byte, error) {
	buf := fr.buf[:0]
	if cap(buf) == 0 {
		buf = make([]byte, 0, frameBufferInitial)
	}
	for {
		if limit > 0 && int64(len(buf)) > limit {
			fr.buf = buf
			return nil, ErrReadLimit
		}
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
	if limit > 0 && int64(len(buf)) > limit {
		fr.buf = buf
		return nil, ErrReadLimit
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
