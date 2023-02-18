package gomux

import (
	"bytes"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// A Stream is a duplex connection multiplexed over a net.Conn. It implements the net.Conn interface.
type Stream struct {
	m       *Mux
	id      uint32
	cond    sync.Cond // guards + synchronizes subsequent fields
	rc, wc  bool      // read closed, write closed
	err     error
	readBuf []byte
	rd, wd  time.Time // deadlines
}

// LocalAddr returns the underlying connection's LocalAddr.
func (s *Stream) LocalAddr() net.Addr { return s.m.conn.LocalAddr() }

// RemoteAddr returns the underlying connection's RemoteAddr.
func (s *Stream) RemoteAddr() net.Addr { return s.m.conn.RemoteAddr() }

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

// consumeFrame stores a frame in s.readBuf and waits for it to be consumed by (*Stream).Read calls.
func (s *Stream) consumeFrame(h frameHeader, payload []byte) {
	switch h.flags {
	case flagCloseWrite:
		s.cond.L.Lock()
		s.wc = true
		s.cond.L.Unlock()

	case flagCloseRead:
		s.cond.L.Lock()
		s.rc = true
		s.cond.L.Unlock()
		s.cond.Broadcast() // wake Read

	case flagCloseStream:
		s.cond.L.Lock()
		s.err = ErrPeerClosedStream
		s.cond.L.Unlock()
		s.cond.Broadcast() // wake Read
		// delete stream from Mux
		s.m.mu.Lock()
		delete(s.m.streams, s.id)
		s.m.mu.Unlock()

	case 0:
		// set payload and wait for it to be consumed
		s.cond.L.Lock()
		s.readBuf = payload
		s.cond.Broadcast() // wake Read
		for len(s.readBuf) > 0 && s.err == nil && !s.rc && (s.rd.IsZero() || time.Now().Before(s.rd)) {
			s.cond.Wait()
		}
		s.cond.L.Unlock()

	default:
		// The flags are mutually exclusive, we should never be here
		// ignore as the peer sent a bad frame
		log.Printf("peer sent invalid frame ID (%v) (length=%v, flags=%v)", h.id, h.length, h.flags)
	}
}

// Read reads data from the Stream.
func (s *Stream) Read(p []byte) (int, error) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	if !s.rd.IsZero() {
		if !time.Now().Before(s.rd) {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.AfterFunc(time.Until(s.rd), s.cond.Broadcast)
		defer timer.Stop()
	}
	for len(s.readBuf) == 0 && s.err == nil && !s.rc && (s.rd.IsZero() || time.Now().Before(s.rd)) {
		s.cond.Wait()
	}
	if s.err != nil {
		if s.err == ErrPeerClosedStream {
			return 0, io.EOF
		}
		return 0, s.err
	} else if s.rc {
		return 0, io.EOF
	} else if !s.rd.IsZero() && !time.Now().Before(s.rd) {
		return 0, os.ErrDeadlineExceeded
	}
	n := copy(p, s.readBuf)
	s.readBuf = s.readBuf[n:]
	s.cond.Broadcast() // wake consumeFrame
	return n, nil
}

// Write writes data to the Stream.
func (s *Stream) Write(p []byte) (int, error) {
	buf := bytes.NewBuffer(p)
	for buf.Len() > 0 {
		// check for error
		s.cond.L.Lock()
		err := s.err
		writeClosed := s.wc
		s.cond.L.Unlock()
		if err != nil {
			return len(p) - buf.Len(), err
		} else if writeClosed {
			return len(p) - buf.Len(), ErrWriteClosed
		}
		// write next frame's worth of data
		payload := buf.Next(maxPayloadSize)
		h := frameHeader{
			id:     s.id,
			length: uint16(len(payload)),
		}
		err = s.m.bufferFrame(h, payload, s.wd)
		if err != nil {
			return len(p) - buf.Len(), err
		}
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
	err := s.m.bufferFrame(h, nil, s.wd)
	if err != nil && err != ErrPeerClosedStream {
		return err
	}

	// delete stream from Mux
	s.m.mu.Lock()
	delete(s.m.streams, s.id)
	s.m.mu.Unlock()
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
	return s.m.bufferFrame(h, nil, s.wd)
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
	return s.m.bufferFrame(h, nil, s.wd)
}

var _ net.Conn = (*Stream)(nil)
