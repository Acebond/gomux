package mux

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

const (
	flagFirst = 1 << iota // first frame in stream
	flagLast              // stream is being closed gracefully
	flagError             // stream is being closed due to an error
)

const (
	idKeepalive    = iota   // empty frame to keep connection open
	idLowestStream = 1 << 8 // IDs below this value are reserved
)

type frameHeader struct {
	id     uint32
	length uint16
	flags  uint16
}

const frameHeaderSize = 4 + 2 + 2
const maxFrameSize = math.MaxUint16 + frameHeaderSize

func encodeFrameHeader(buf []byte, h frameHeader) {
	binary.LittleEndian.PutUint32(buf[0:], h.id)
	binary.LittleEndian.PutUint16(buf[4:], h.length)
	binary.LittleEndian.PutUint16(buf[6:], h.flags)
}

func decodeFrameHeader(buf []byte) frameHeader {
	return frameHeader{
		id:     binary.LittleEndian.Uint32(buf[0:]),
		length: binary.LittleEndian.Uint16(buf[4:]),
		flags:  binary.LittleEndian.Uint16(buf[6:]),
	}
}

func appendFrame(buf []byte, h frameHeader, payload []byte) []byte {
	frame := buf[len(buf):][:frameHeaderSize+len(payload)]
	encodeFrameHeader(frame[:frameHeaderSize], h)
	copy(frame[frameHeaderSize:], payload)
	return buf[:len(buf)+len(frame)]
}

type frameReader struct {
	r   io.Reader
	buf []byte
}

// nextFrame reads a frame from conn
func (fr *frameReader) nextFrame() (frameHeader, []byte, error) {
	// Read header
	//hdrBuf := make([]byte, frameHeaderSize)
	if _, err := io.ReadFull(fr.r, fr.buf[:frameHeaderSize]); err != nil {
		return frameHeader{}, nil, fmt.Errorf("could not read frame header: %w", err)
	}
	h := decodeFrameHeader(fr.buf[:frameHeaderSize])

	//payload := make([]byte, h.length)

	if _, err := io.ReadFull(fr.r, fr.buf[:h.length]); err != nil {
		return frameHeader{}, nil, fmt.Errorf("could not read frame payload: %w", err)
	}
	return h, fr.buf[:h.length], nil
}
