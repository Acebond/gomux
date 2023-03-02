package gomux

import (
	"bytes"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/Acebond/gomux/ringbuffer"
)

// A Stream is a duplex connection multiplexed over a net.Conn. It implements
// the net.Conn interface.
type Stream struct {
	mux        *Mux
	id         uint32
	cond       sync.Cond // guards + synchronizes subsequent fields
	rc, wc     bool      // read closed, write closed
	err        error
	windowSize uint32
	readBuf    *ringbuffer.Buffer
	rd, wd     time.Time // deadlines
}

func newStream(id uint32, m *Mux) *Stream {
	return &Stream{
		mux:        m,
		id:         id,
		cond:       sync.Cond{L: new(sync.Mutex)},
		err:        m.err,
		windowSize: windowSize,
		readBuf:    ringbuffer.NewBuffer(windowSize), // must be same as windowSize
	}
}

// LocalAddr returns the underlying connection's LocalAddr.
func (s *Stream) LocalAddr() net.Addr { return s.mux.conn.LocalAddr() }

// RemoteAddr returns the underlying connection's RemoteAddr.
func (s *Stream) RemoteAddr() net.Addr { return s.mux.conn.RemoteAddr() }

// SetDeadline sets the read and write deadlines associated with the Stream. It
// is equivalent to calling both SetReadDeadline and SetWriteDeadline.
//
// This implementation does not entirely conform to the net.Conn interface:
// setting a new deadline does not affect pending Read or Write calls, only
// future calls.
func (s *Stream) SetDeadline(t time.Time) error {
	s.SetReadDeadline(t)
	s.SetWriteDeadline(t)
	return nil
}

// SetReadDeadline sets the read deadline associated with the Stream.
//
// This implementation does not entirely conform to the net.Conn interface:
// setting a new deadline does not affect pending Read calls, only future calls.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	s.rd = t
	return nil
}

// SetWriteDeadline sets the write deadline associated with the Stream.
//
// This implementation does not entirely conform to the net.Conn interface:
// setting a new deadline does not affect pending Write calls, only future
// calls.
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	s.wd = t
	return nil
}

// consumeFrame processes a frame based on h.flags.
func (s *Stream) consumeFrame(h frameHeader, payload []byte) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	switch h.flags {
	case flagCloseWrite:
		s.wc = true

	case flagCloseRead:
		s.rc = true

	case flagCloseStream:
		s.err = ErrPeerClosedStream
		s.mux.deleteStream(s.id)

	case flagWindowUpdate:
		s.windowSize += h.length

	case flagData:
		s.readBuf.Write(payload)
	}

	s.cond.Broadcast() // wake Read/Write
}

// Read reads data from the Stream.
func (s *Stream) Read(p []byte) (n int, err error) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	defer func() {
		if err == nil && n > 0 {
			// Tell sender bytes have been consumed
			h := frameHeader{
				id:     s.id,
				length: uint32(n),
				flags:  flagWindowUpdate,
			}
			s.mux.bufferFrame(h, nil)
		}
	}()

	if !s.rd.IsZero() {
		if !time.Now().Before(s.rd) {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.AfterFunc(time.Until(s.rd), s.cond.Broadcast)
		defer timer.Stop()
	}

	for s.readBuf.Len() == 0 && s.err == nil && !s.rc && (s.rd.IsZero() || time.Now().Before(s.rd)) {
		s.cond.Wait()
	}

	// Determine how we left the for loop. If data has been buffered it always
	// takes precedence over errors. This is because a sender could have sent
	// some data then closed the stream. We want to return the data before
	// indicating the closure of the stream to keep the order of events correct.
	if s.readBuf.Len() > 0 {
		n, _ := s.readBuf.Read(p)
		return n, nil
	} else if s.err != nil {
		if s.err == ErrPeerClosedStream {
			return 0, io.EOF
		}
		return 0, s.err
	} else if s.rc {
		return 0, io.EOF
	} else if !s.rd.IsZero() && !time.Now().Before(s.rd) {
		return 0, os.ErrDeadlineExceeded
	}
	panic("impossible")
}

// Write writes data to the Stream.
func (s *Stream) Write(p []byte) (int, error) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	buf := bytes.NewBuffer(p)
	for buf.Len() > 0 {

		payload := buf.Next(maxPayloadSize)
		h := frameHeader{
			id:     s.id,
			length: uint32(len(payload)),
		}

		for h.length > s.windowSize && s.err == nil && !s.wc && (s.wd.IsZero() || time.Now().Before(s.wd)) {
			s.cond.Wait()
		}

		// Determine how we left the for loop.
		if s.err != nil {
			return len(p) - buf.Len() - len(payload), s.err
		} else if s.wc {
			return len(p) - buf.Len() - len(payload), ErrWriteClosed
		} else if !s.wd.IsZero() && !time.Now().Before(s.wd) {
			return 0, os.ErrDeadlineExceeded
		}

		// write next frame's worth of data
		err := s.mux.bufferFrame(h, payload)
		if err != nil {
			return len(p) - buf.Len() - len(payload), err
		}
		s.windowSize -= h.length
	}
	return len(p), nil
}

// Close closes the Stream. The underlying connection is not closed.
func (s *Stream) Close() error {
	// cancel outstanding Read/Write calls
	//
	// NOTE: Read calls will be interrupted immediately, but Write calls might
	// send another frame before observing the Close. This is ok: the peer will
	// discard any frames that arrive after the flagLast frame.
	s.cond.L.Lock()
	if s.err == ErrClosedStream || s.err == ErrPeerClosedStream {
		s.cond.L.Unlock()
		return nil
	}
	s.err = ErrClosedStream
	s.cond.Broadcast()
	s.cond.L.Unlock()

	h := frameHeader{
		id:    s.id,
		flags: flagCloseStream,
	}
	err := s.mux.bufferFrame(h, nil)
	if err != nil && err != ErrPeerClosedStream {
		return err
	}

	// delete stream from Mux
	s.mux.deleteStream(s.id)
	return nil
}

// CloseWrite shuts down the writing side of the stream. Most callers should just use Close.
func (s *Stream) CloseWrite() error {
	s.cond.L.Lock()
	s.wc = true
	s.cond.L.Unlock()

	// Signal peer that no more data is coming
	h := frameHeader{
		id:    s.id,
		flags: flagCloseRead,
	}
	return s.mux.bufferFrame(h, nil)
}

// CloseRead shuts down the reading side of the stream. Most callers should just use Close.
func (s *Stream) CloseRead() error {
	s.cond.L.Lock()
	s.rc = true
	s.cond.L.Unlock()

	// Signal peer that no more data will be accepted
	h := frameHeader{
		id:    s.id,
		flags: flagCloseWrite,
	}
	return s.mux.bufferFrame(h, nil)
}

var _ net.Conn = (*Stream)(nil)
