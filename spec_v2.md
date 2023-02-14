## Full Spec

A gomux session is an exchange of *frames* between two peers over a shared
connection. A session is initiated by a handshake, and terminated (gracefully)
when a "final" frame is sent, or (forcibly) when the connection is closed.

All integers in this spec are little-endian.

### Frames

A frame consists of a *frame header* followed by a payload. A header is:

| Length | Type   | Description    |
|--------|--------|----------------|
|   4    | uint32 | ID             |
|   2    | uint16 | Payload length |
|   2    | uint16 | Flags          |

The ID specifies which *stream* a frame belongs to. Streams are numbered
sequentially, starting at 256. To prevent collisions, streams initiated by the
dialing peer use even IDs, while the accepting peer uses odd IDs.

An ID of 0 indicates a keepalive frame. Keepalives contain no payload and merely
serve to keep the underlying connection open.

There are three defined flags:

| Bit | Description           |
|-----|-----------------------|
|  0  | First frame in stream |
|  1  | Last frame in stream  |
|  2  | Error                 |

The "Error" flag may only be set alongside the "Last frame" flag, and indicates
that the payload contains a string describing why the stream was closed.
