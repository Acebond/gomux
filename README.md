# Gomux

Gomux is a high-performance stream multiplexer inspired from [SiaMux](https://github.com/SiaFoundation/mux). It allows you to operate many distinct bidirectional streams on top of a single underlying connection. It transparently encrypts all data using XChaCha20-Poly1305 with a pre-shared key.

## Specification

A gomux session is an exchange of *frames* between two peers over a shared connection.

### Frames

All integers in this spec are little-endian.

A frame consists of a *frame header* followed by a payload. A header is 8 bytes and defined as:

| Bits | Type   | Name     | Description |
|------|--------|----------|-------------|
| 32   | uint32 | ID       | The ID specifies which *stream* a frame belongs to. Streams are numbered sequentially, starting at 0. To prevent collisions, streams initiated by the client peer use even IDs, while the server peer uses odd IDs. |
| 32   | uint32 | Length   | The length specifies the length of the payload or window update. |
| 32   | uint32 | Flags    | Determines the type of frame and how it should be processed. |

The flags are defined as:

| Value | Name         | Description |
|-------|--------------|-------------|
|   0   | Data         | Indicates a payload of size length follows the frame. |
|   1   | Keepalive    | Indicates a keepalive frame. Contain no payload and merely serve to keep the underlying connection open.|
|   2   | OpenStream   | Indicates to the accepting peer the creation of a new stream. |
|   3   | CloseRead    | Closes the stream for reading. |
|   4   | CloseWrite   | Closes the stream for writing. |
|   5   | CloseStream  | Indicates that the stream has been closed by the peer. |
|   6   | WindowUpdate | Indicates to the peer that length bytes have been read from the stream ID read buffer. |
|   7   | CloseMux     | Indicates the shutdown of the stream multiplexer. |
