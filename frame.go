package gomux

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

type frameHeader struct {
	id     uint32
	length uint16
	flags  uint16
}

const (
	frameHeaderSize = 4 + 2 + 2
	maxPayloadSize  = math.MaxUint16
	maxFrameSize    = frameHeaderSize + maxPayloadSize
	writeBufferSize = maxFrameSize * 10
	windowSize      = maxPayloadSize
)

const (
	flagKeepalive    = 1 << iota // empty frame to keep connection open
	flagOpenStream               // first frame in stream
	flagCloseRead                // shuts down the reading side of the stream
	flagCloseWrite               // shuts down the writing side of the stream
	flagCloseStream              // stream is being closed gracefully
	flagWindowUpdate             // used to updated the read window size
	flagCloseMux                 // mux is being closed gracefully
)

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
	reader  io.Reader
	header  []byte
	payload []byte
}

// nextFrame reads a frame from reader
func (fr *frameReader) nextFrame() (frameHeader, []byte, error) {
	if _, err := io.ReadFull(fr.reader, fr.header); err != nil {
		return frameHeader{}, nil, fmt.Errorf("could not read frame header: %w", err)
	}
	h := decodeFrameHeader(fr.header)

	if h.flags == 0 {
		if _, err := io.ReadFull(fr.reader, fr.payload[:h.length]); err != nil {
			return frameHeader{}, nil, fmt.Errorf("could not read frame payload: %w", err)
		}
		return h, fr.payload[:h.length], nil
	} else {
		return h, nil, nil
	}
}
