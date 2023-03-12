package gomux

import (
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/chacha20poly1305"
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
	ErrPeerClosedConn   = errors.New("peer closed mux gracefully")
	ErrWriteClosed      = errors.New("write end of stream closed")
)

// A Mux multiplexes multiple duplex Streams onto a single net.Conn.
type Mux struct {
	conn       net.Conn
	acceptChan chan *Stream
	aead       cipher.AEAD
	mu         sync.Mutex // all subsequent fields are guarded by mu
	writeCond  sync.Cond
	bufferCond sync.Cond // separate cond for waking a single bufferFrame
	streams    map[uint32]*Stream
	nextID     uint32
	err        error // sticky and fatal
	writeBuf   []byte
	sendBuf    []byte
	writeBufA  []byte
	writeBufB  []byte
}

func (m *Mux) appendFrame(buf []byte, h frameHeader, payload []byte) []byte {
	rand.Read(h.nonce[:])
	frame := buf[len(buf):][:frameHeaderSize+len(payload)+m.aead.Overhead()]
	encodeFrameHeader(frame[:frameHeaderSize], h)
	m.aead.Seal(frame[frameHeaderSize:][:0], h.nonce[:], payload, frame[:frameHeaderSize])
	//log.Println(check)
	// copy(frame[frameHeaderSize:], payload)
	return buf[:len(buf)+len(frame)]
}

// setErr sets the Mux error and wakes up all Mux-related goroutines. If m.err
// is already set, setErr is a no-op.
func (m *Mux) setErr(err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}

	// set sticky error, close conn, and wake everyone up
	m.err = err
	for _, s := range m.streams {
		s.cond.L.Lock()
		s.err = err
		s.cond.L.Unlock()
		s.cond.Broadcast()
	}
	m.conn.Close()
	m.writeCond.Signal()
	m.bufferCond.Broadcast()
	close(m.acceptChan)
	return err
}

// bufferFrame blocks until it can store its frame in m.writeBuf. It returns
// early with an error if m.err is set.
func (m *Mux) bufferFrame(h frameHeader, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// block until we can add the frame to the buffer
	for len(m.writeBuf)+frameHeaderSize+len(payload)+m.aead.Overhead() > cap(m.writeBuf) && m.err == nil {
		m.bufferCond.Wait()
	}
	if m.err != nil {
		return m.err
	}

	// queue our frame and wake the writeLoop
	m.writeBuf = m.appendFrame(m.writeBuf, h, payload)
	m.writeCond.Signal()

	// wake at most one bufferFrame call
	m.bufferCond.Signal()
	return nil
}

// writeLoop handles the actual Writes to the Mux's net.Conn. It waits for
// bufferFrame calls to fill m.writeBuf, then flushes the buffer to the
// underlying connection. It also handles keepalives.
func (m *Mux) writeLoop() {
	// wake cond whenever a keepalive is due
	keepaliveInterval := time.Minute * 10
	nextKeepalive := time.Now().Add(keepaliveInterval)
	timer := time.AfterFunc(keepaliveInterval, m.writeCond.Signal)
	defer timer.Stop()

	for {
		m.mu.Lock()
		for len(m.writeBuf) == 0 && m.err == nil && time.Now().Before(nextKeepalive) {
			m.writeCond.Wait()
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
			m.writeBuf = m.appendFrame(m.writeBuf[:0], frameHeader{flags: flagKeepalive}, nil)
		}

		// to avoid blocking bufferFrame while we Write, swap writeBufA and writeBufB
		m.writeBuf, m.sendBuf = m.sendBuf, m.writeBuf

		// wake at most one bufferFrame call
		m.mu.Unlock()
		m.bufferCond.Signal()

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

// Delete stream from Mux
func (m *Mux) deleteStream(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	m.mu.Unlock()
}

// readLoop handles the actual Reads from the Mux's net.Conn. It waits for a
// frame to arrive, then routes it to the appropriate Stream, creating a new
// Stream if none exists.
func (m *Mux) readLoop() {

	fr := &frameReader{
		reader:  m.conn,
		header:  make([]byte, frameHeaderSize),
		payload: make([]byte, maxPayloadSize+m.aead.Overhead()),
	}

	for {
		h, payload, err := fr.nextFrame(m.aead)

		if err != nil {
			m.setErr(err)
			return
		}

		switch h.flags {
		case flagKeepalive:
			// no action required

		case flagOpenStream:
			s := newStream(h.id, m)
			m.mu.Lock()
			m.streams[h.id] = s
			m.mu.Unlock()
			m.acceptChan <- s

		case flagCloseMux:
			m.setErr(ErrPeerClosedConn)
			return

		default:
			m.mu.Lock()
			stream, found := m.streams[h.id]
			m.mu.Unlock()
			if found {
				stream.consumeFrame(h, payload)
			}
			// else received frame for assumed to be closed stream so ignore it.
		}
	}
}

// Close closes the underlying net.Conn.
func (m *Mux) Close() error {
	// tell perr we are shutting down
	h := frameHeader{flags: flagCloseMux}
	m.bufferFrame(h, nil)

	// if there's a buffered Write, wait for it to be sent
	m.mu.Lock()
	for len(m.writeBuf) > 0 && m.err == nil {
		m.bufferCond.Wait()
	}
	m.mu.Unlock()
	err := m.setErr(ErrClosedConn)
	if err == ErrClosedConn {
		err = nil
	}
	return err
}

// AcceptStream waits for and returns the next peer-initiated Stream.
func (m *Mux) AcceptStream() (*Stream, error) {
	if s, ok := <-m.acceptChan; ok {
		return s, nil
	}
	return nil, m.err
}

// OpenStream creates a new Stream.
func (m *Mux) OpenStream() (*Stream, error) {
	m.mu.Lock()
	s := newStream(m.nextID, m)
	m.streams[s.id] = s
	m.nextID += 2 // int wraparound intended
	m.mu.Unlock()

	// send flagOpenStream to tell peer the stream exists
	h := frameHeader{
		id:    s.id,
		flags: flagOpenStream,
	}
	return s, m.bufferFrame(h, nil)
}

// newMux initializes a Mux and spawns its readLoop and writeLoop goroutines.
func newMux(conn net.Conn, psk string) *Mux {
	m := &Mux{
		conn:       conn,
		streams:    make(map[uint32]*Stream),
		acceptChan: make(chan *Stream, 256),
		writeBufA:  make([]byte, 0, writeBufferSize),
		writeBufB:  make([]byte, 0, writeBufferSize),
	}
	m.writeBuf = m.writeBufA // initial writeBuf is writeBufA
	m.sendBuf = m.writeBufB  // initial sendBuf is writeBufB
	// both conds use the same mutex
	m.writeCond.L = &m.mu
	m.bufferCond.L = &m.mu

	key := blake2b.Sum256([]byte(psk))
	var err error
	m.aead, err = chacha20poly1305.NewX(key[:])
	if err != nil {
		panic(err)
	}

	go m.readLoop()
	go m.writeLoop()
	return m
}

// Client creates and initializes a new client-side Mux on the provided conn.
// Client takes overship of the conn.
func Client(conn net.Conn) (*Mux, error) {
	return newMux(conn, "test"), nil
}

// Server creates and initializes a new server-side Mux on the provided conn.
// Server takes overship of the conn.
func Server(conn net.Conn) (*Mux, error) {
	m := newMux(conn, "test")
	m.nextID++ // avoid collisions with client peer
	return m, nil
}
