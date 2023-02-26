package gomux

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

type frameHeader struct {
	id     uint32
	length uint32
	flags  uint8
}

const (
	frameHeaderSize = 4 + 2 + 2 // uint32 + (uint24 + uint8)
	maxPayloadSize  = math.MaxUint16
	maxFrameSize    = frameHeaderSize + maxPayloadSize
	writeBufferSize = maxFrameSize * 10
	windowSize      = maxPayloadSize
)

const (
	flagData         = iota // data frame
	flagKeepalive           // empty frame to keep connection open
	flagOpenStream          // first frame in stream
	flagCloseRead           // shuts down the reading side of the stream
	flagCloseWrite          // shuts down the writing side of the stream
	flagCloseStream         // stream is being closed gracefully
	flagWindowUpdate        // used to updated the read window size
	flagCloseMux            // mux is being closed gracefully
)

func encodeFrameHeader(buf []byte, h frameHeader) {
	binary.LittleEndian.PutUint32(buf[0:], h.id)
	binary.LittleEndian.PutUint32(buf[4:], h.length|uint32(h.flags)<<24)
}

func decodeFrameHeader(buf []byte) (h frameHeader) {
	return frameHeader{
		id:     uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24,
		length: uint32(buf[4]) | uint32(buf[5])<<8 | uint32(buf[6])<<16,
		flags:  buf[7],
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
