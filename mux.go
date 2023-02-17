package gomux

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

// NOTE: This package makes heavy use of sync.Cond to manage concurrent streams
// multiplexed onto a single connection. sync.Cond is rarely used (since it is
// almost never the right tool for the job), and consequently, Go programmers
// tend to be unfamiliar with its semantics. Nevertheless, it is currently the
// only way to achieve optimal throughput in a stream multiplexer, so we make
// careful use of it here. Please make sure you understand sync.Cond thoroughly
// before attempting to modify this code.

// Errors relating to stream or mux shutdown.
var (
	ErrClosedConn       = errors.New("underlying connection was closed")
	ErrClosedStream     = errors.New("stream was gracefully closed")
	ErrPeerClosedStream = errors.New("peer closed stream gracefully")
	ErrPeerClosedConn   = errors.New("peer closed underlying connection")
)

// A Mux multiplexes multiple duplex Streams onto a single net.Conn.
type Mux struct {
	conn       net.Conn
	acceptChan chan *Stream
	mu         sync.Mutex // all subsequent fields are guarded by mu
	cond       sync.Cond
	streams    map[uint32]*Stream
	nextID     uint32
	err        error // sticky and fatal
	writeBuf   []byte
	sendBuf    []byte
	writeBufA  []byte
	writeBufB  []byte
	bufferCond sync.Cond // separate cond for waking a single bufferFrame
}

// setErr sets the Mux error and wakes up all Mux-related goroutines. If m.err
// is already set, setErr is a no-op.
func (m *Mux) setErr(err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}

	// try to detect when the peer closed the connection
	if isConnCloseError(err) {
		err = ErrPeerClosedConn
	}

	// set sticky error, close conn, and wake everyone up
	m.err = err
	for _, s := range m.streams {
		s.cond.L.Lock()
		s.err = err
		s.cond.Broadcast()
		s.cond.L.Unlock()
	}
	m.conn.Close()
	close(m.acceptChan)
	m.cond.Broadcast()
	m.bufferCond.Broadcast()
	return err
}

// bufferFrame blocks until it can store its frame in m.writeBuf.
// It returns early with an error if m.err is set or if the deadline expires.
func (m *Mux) bufferFrame(h frameHeader, payload []byte, deadline time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !deadline.IsZero() {
		if !time.Now().Before(deadline) {
			return os.ErrDeadlineExceeded
		}
		timer := time.AfterFunc(time.Until(deadline), m.bufferCond.Broadcast) // nice
		defer timer.Stop()
	}
	// block until we can add the frame to the buffer
	for len(m.writeBuf)+frameHeaderSize+len(payload) > cap(m.writeBuf) && m.err == nil && (deadline.IsZero() || time.Now().Before(deadline)) {
		m.bufferCond.Wait()
	}
	if m.err != nil {
		return m.err
	} else if !deadline.IsZero() && !time.Now().Before(deadline) {
		return os.ErrDeadlineExceeded
	}
	// queue our frame and wake the writeLoop
	//
	// NOTE: it is not necessary to wait for the writeLoop to flush our frame.
	// After all, a successful write() syscall doesn't mean that the peer
	// actually received the data, just that the packets are sitting in a kernel
	// buffer somewhere.
	m.writeBuf = appendFrame(m.writeBuf, h, payload)
	m.cond.Broadcast()

	//if covert {
	// wake all other bufferFrame calls
	//
	// NOTE: this causes lots of spurious wakeups. Covert bandwidth is
	// precious, though, so it's better to take a small performance hit to
	// ensure that we're making use of whatever bandwidth is available.
	// If it becomes a problem, we can easily add a separate sync.Cond.
	//m.bufferCond.Broadcast()
	//} else {
	// wake at most one bufferFrame call
	//
	// NOTE: it's possible that we'll wake the "wrong" bufferFrame call, i.e.
	// one whose payload is too large to fit in the buffer. This means we won't
	// buffer any additional frames until the writeLoop flushes the buffer.
	// Calling Broadcast instead of Signal prevents this, but also incurs a
	// massive performance penalty when there are many concurrent streams. We
	// could probably get the best of both worlds with a more sophisticated
	// buffering strategy, but the current implementation is fast enough.
	m.bufferCond.Signal()
	//}
	return nil
}

// writeLoop handles the actual Writes to the Mux's net.Conn. It waits for
// bufferFrame calls to fill m.writeBuf, then flushes the buffer to the
// underlying connection. It also handles keepalives.
func (m *Mux) writeLoop() {
	// wake cond whenever a keepalive is due
	// NOTE: we send a keepalive when 75% of the MaxTimeout has elapsed
	maxTimeout := 20 * time.Minute
	keepaliveInterval := maxTimeout - maxTimeout/4
	nextKeepalive := time.Now().Add(keepaliveInterval)
	timer := time.AfterFunc(keepaliveInterval, m.cond.Broadcast)
	defer timer.Stop()

	for {
		m.mu.Lock()
		for len(m.writeBuf) == 0 && m.err == nil && time.Now().Before(nextKeepalive) {
			m.cond.Wait()
		}
		if m.err != nil {
			m.mu.Unlock()
			return
		}

		// if we have a normal frame, use that; otherwise, send a keepalive
		//
		// NOTE: even if we were woken by the keepalive timer, there might be a
		// normal frame ready to send, in which case we don't need a keepalive
		if len(m.writeBuf) == 0 {
			m.writeBuf = appendFrame(m.writeBuf[:0], frameHeader{flags: flagKeepalive}, nil)
		}

		// to avoid blocking bufferFrame while we Write, swap writeBufA and writeBufB
		m.writeBuf, m.sendBuf = m.sendBuf, m.writeBuf

		// wake at most one bufferFrame call
		m.bufferCond.Signal()
		m.mu.Unlock()

		// reset keepalive timer
		timer.Stop()
		timer.Reset(keepaliveInterval)
		nextKeepalive = time.Now().Add(keepaliveInterval)

		// write the packet(s)
		if _, err := m.conn.Write(m.sendBuf); err != nil {
			m.setErr(err)
			return
		}

		// clear sendBuf
		m.sendBuf = m.sendBuf[:0]

	}
}

// readLoop handles the actual Reads from the Mux's net.Conn. It waits for a
// frame to arrive, then routes it to the appropriate Stream, creating a new
// Stream if none exists. It then waits for the frame to be fully consumed by
// the Stream before attempting to Read again.
func (m *Mux) readLoop() {
	var curStream *Stream // saves a lock acquisition + map lookup in the common case

	fr := &frameReader{
		reader:  m.conn,
		header:  make([]byte, frameHeaderSize),
		payload: make([]byte, maxPayloadSize),
	}

	for {
		h, payload, err := fr.nextFrame()

		if err != nil {
			m.setErr(err)
			return
		}

		if h.flags&flagKeepalive == flagKeepalive {
			continue // no action required
		}

		// look for matching Stream
		if curStream == nil || h.id != curStream.id {
			m.mu.Lock()
			if s := m.streams[h.id]; s != nil {
				curStream = s
			} else {
				if h.flags&flagFirst == 0 {
					// we don't recognize the frame's ID, but it's not the
					// first frame of a new stream either; we must have
					// already closed the stream this frame belongs to, so
					// ignore it
					m.mu.Unlock()
					continue
				}
				// create a new stream
				curStream = &Stream{
					m:  m,
					id: h.id,
					// needAccept: true,
					cond: sync.Cond{L: new(sync.Mutex)},
				}
				m.streams[h.id] = curStream

				// m.cond.Broadcast() // wake (*Mux).AcceptStream
				m.acceptChan <- curStream

			}
			m.mu.Unlock()
		}
		curStream.consumeFrame(h, payload)
	}
}

// Close closes the underlying net.Conn.
func (m *Mux) Close() error {
	// if there's a buffered Write, wait for it to be sent
	m.mu.Lock()
	for len(m.writeBuf) != 0 && m.err == nil {
		m.bufferCond.Wait()
	}
	m.mu.Unlock()
	err := m.setErr(ErrClosedConn)
	if err == ErrClosedConn || err == ErrPeerClosedConn {
		err = nil
	}
	return err
}

// AcceptStream waits for and returns the next peer-initiated Stream.
func (m *Mux) AcceptStream() (*Stream, error) {
	if s, ok := <-m.acceptChan; ok {
		return s, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil, m.err
}

// OpenStream creates a new Stream.
func (m *Mux) OpenStream() (*Stream, error) {
	m.mu.Lock()

	s := &Stream{
		m:  m,
		id: m.nextID,
		//needAccept: false,
		cond: sync.Cond{L: new(sync.Mutex)},
		err:  m.err, // stream is unusable if m.err is set
	}
	m.streams[s.id] = s

	// nextID will wraparound when it grows too large
	m.nextID += 2
	m.mu.Unlock()

	// send First frame to tell peer the stream exists
	h := frameHeader{
		id:     s.id,
		length: 0,
		flags:  flagFirst,
	}

	return s, s.m.bufferFrame(h, nil, s.wd)
}

// newMux initializes a Mux and spawns its readLoop and writeLoop goroutines.
func newMux(conn net.Conn) *Mux {
	m := &Mux{
		conn:       conn,
		streams:    make(map[uint32]*Stream),
		acceptChan: make(chan *Stream, 128),
		writeBufA:  make([]byte, 0, maxFrameSize*10),
		writeBufB:  make([]byte, 0, maxFrameSize*10),
	}
	m.writeBuf = m.writeBufA // initl writeBuf is A
	m.sendBuf = m.writeBufB
	// both conds use the same mutex
	m.cond.L = &m.mu
	m.bufferCond.L = &m.mu
	go m.readLoop()
	go m.writeLoop()
	return m
}

// Dial initiates a mux protocol handshake on the provided conn.
func Client(conn net.Conn) (*Mux, error) {
	return newMux(conn), nil
}

// DialStreamContext creates a new Stream with the provided context. When the
// context expires, the Stream will be closed and any pending calls will return
// ctx.Err(). DialStreamContext spawns a goroutine whose lifetime matches that
// of the context.
//
// Unlike e.g. net.Dial, this does not perform any I/O; the peer will not be
// aware of the new Stream until Write is called.
func (m *Mux) OpenStreamContext(ctx context.Context) (*Stream, error) {
	s, err := m.OpenStream()
	go func() {
		<-ctx.Done()
		s.cond.L.Lock()
		defer s.cond.L.Unlock()
		if ctx.Err() != nil && s.err == nil {
			s.err = ctx.Err()
			s.cond.Broadcast()
		}
	}()
	return s, err
}

// Accept reciprocates a mux protocol handshake on the provided conn.
func Server(conn net.Conn) (*Mux, error) {
	m := newMux(conn)
	m.nextID++ // avoid collisions with Dialing peer
	return m, nil
}
