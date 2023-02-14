## Specification

A gomux session is an exchange of *frames* between two peers over a shared
connection.

### Frames

All integers in this spec are little-endian.

A frame consists of a *frame header* followed by a payload. A header is:

| Length | Type   | Description    |
|--------|--------|----------------|
|   4    | uint32 | ID             |
|   2    | uint16 | Payload length |
|   2    | uint16 | Flags          |

The ID specifies which *stream* a frame belongs to. Streams are numbered
sequentially, starting at 0. To prevent collisions, streams initiated by the
dialing peer use even IDs, while the accepting peer uses odd IDs.

The payload length specifies the length of the payload.

The flags are defined as:

| Bit | Description           |
|-----|-----------------------|
|  0  | First frame in stream |
|  1  | Last frame in stream  |
|  2  | Error                 |
|  3  | Keepalive frame       |

The "First" flag indicates to the accepting peer the creation of a new stream.

The "Last" flag indicates that the stream is closed.

The "Error" flag may only be set alongside the "Last frame" flag, and indicates
that the payload contains a string describing why the stream was closed.

The "Keepalive" flag indicates a keepalive frame. Keepalives contain no payload and merely
serve to keep the underlying connection open.
