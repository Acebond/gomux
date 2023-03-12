package gomux

import (
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

type frameHeader struct {
	id     uint32
	length uint32 // only first 24 bits are used
	flags  uint8
	nonce  [chacha20poly1305.NonceSizeX]byte
}

const (
	frameHeaderSize = 4 + 2 + 2 + chacha20poly1305.NonceSizeX
	maxPayloadSize  = 1 << 16 // must be < 2 ^ 24
	maxFrameSize    = frameHeaderSize + maxPayloadSize + chacha20poly1305.Overhead
	writeBufferSize = maxFrameSize * 10 // must be >= maxFrameSize
	windowSize      = maxPayloadSize    // must be >= maxPayloadSize
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
	copy(buf[8:], h.nonce[:])
}

func decodeFrameHeader(buf []byte) frameHeader {
	h := frameHeader{
		id:     uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24,
		length: uint32(buf[4]) | uint32(buf[5])<<8 | uint32(buf[6])<<16,
		flags:  buf[7],
	}
	copy(h.nonce[:], buf[8:])
	return h
}

type frameReader struct {
	reader  io.Reader
	header  []byte
	payload []byte
}

// nextFrame reads a frame from reader
func (fr *frameReader) nextFrame(aead cipher.AEAD) (frameHeader, []byte, error) {
	if _, err := io.ReadFull(fr.reader, fr.header); err != nil {
		return frameHeader{}, nil, fmt.Errorf("could not read frame header: %w", err)
	}
	h := decodeFrameHeader(fr.header)

	payloadSize := aead.Overhead()
	if h.flags == flagData {
		payloadSize += int(h.length)
	}

	//if h.length > maxPayloadSize {
	//	return frameHeader{}, nil, fmt.Errorf("header length (%v) > maxPayloadSize (%v)", h.length, maxPayloadSize)
	//}

	if _, err := io.ReadFull(fr.reader, fr.payload[:payloadSize]); err != nil {
		return frameHeader{}, nil, fmt.Errorf("could not read frame payload: %w", err)
	}
	//return h, fr.payload[:h.length], nil

	// Decrypt the message and check it wasn't tampered with.
	_, err := aead.Open(fr.payload[:0], h.nonce[:], fr.payload[:payloadSize], fr.header)
	if err != nil {
		panic(err)
	}

	return h, fr.payload[:h.length], nil
}
