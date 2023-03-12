package gomux

import (
	"crypto/cipher"
	"crypto/rand"
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

// nextFrame reads and decrypts a frame from reader
func readFrame(reader io.Reader, aead cipher.AEAD, frameBuf []byte) (frameHeader, []byte, error) {

	if _, err := io.ReadFull(reader, frameBuf[:frameHeaderSize]); err != nil {
		return frameHeader{}, nil, fmt.Errorf("could not read frame header: %w", err)
	}
	h := decodeFrameHeader(frameBuf[:frameHeaderSize])

	payloadSize := uint32(chacha20poly1305.Overhead)
	if h.flags == flagData {
		payloadSize += h.length
	}

	if _, err := io.ReadFull(reader, frameBuf[frameHeaderSize:][:payloadSize]); err != nil {
		return frameHeader{}, nil, fmt.Errorf("could not read frame payload: %w", err)
	}

	// Decrypt the message and check it wasn't tampered with.
	_, err := aead.Open(frameBuf[frameHeaderSize:][:0], h.nonce[:], frameBuf[frameHeaderSize:][:payloadSize], frameBuf[:frameHeaderSize])
	return h, frameBuf[frameHeaderSize:][:h.length], err
}

// appendFrame writs and encrypts a frame to buf
func appendFrame(buf []byte, aead cipher.AEAD, h frameHeader, payload []byte) []byte {
	rand.Read(h.nonce[:])
	frame := buf[len(buf):][:frameHeaderSize+len(payload)+aead.Overhead()]
	encodeFrameHeader(frame[:frameHeaderSize], h)
	aead.Seal(frame[frameHeaderSize:][:0], h.nonce[:], payload, frame[:frameHeaderSize])
	return buf[:len(buf)+len(frame)]
}
