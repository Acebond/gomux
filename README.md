# Gomux

Gomux is a high-performance stream multiplexer forked from [SiaMux](https://github.com/SiaFoundation/mux). It allows you to operate many distinct bidirectional streams on top of a single underlying connection. Gomux follows the Unix philosophy of "do one thing and do it well" and unlike SiaMux does not offer encryption, authentication or covert streams. Encryption and  authentication can if required be implemented on the underlying connection used by Gomux.

## Benchmark
Gomux on an i5-13600K can transfer approximately 4500 MB/s. The number of streams does not impact performance. 

The benchmark below does show slightly better throughput with a larger (100+) number of streams. This is because having more readers/writers means less time is spent blocking waiting for data to be read or written to the streams.

Profiling the benchmark showed that 9.62s (80.10%) of time was spent inside the WSARecv() and WSASend() syscalls. This indicates that the majority of time is spent waiting for the OS (in this case Windows) kernel to process the reads and writes to the underlying (in this case TCP) connection. These functions are likely implemented to be very performant, and are simply blocked due to the bandwidth of the underlying connection being maxed out. 

The conclusion is that the performance will be limited by the throughput of the underlying connection.

Keep in mind that the Golang garbage collector pauses can cause different results in benchmarks. The same benchmark run multiple times can sometimes show a lower transfer speed due to an unlucky garbage collector pause.

```
PS C:\Users\acebond\go\src\github.com\Acebond\gomux> go test -bench=BenchmarkMux
goos: windows
goarch: amd64
pkg: github.com/Acebond/gomux
cpu: 13th Gen Intel(R) Core(TM) i5-13600K
BenchmarkMux/1-20          65256             15717 ns/op        4169.58 MB/s         63655 frames/sec          0 B/op          0 allocs/op
BenchmarkMux/2-20          39043             29309 ns/op        4471.95 MB/s         68238 frames/sec          0 B/op          0 allocs/op
BenchmarkMux/10-20          8221            146447 ns/op        4475.00 MB/s         68284 frames/sec          2 B/op          0 allocs/op
BenchmarkMux/100-20                  830           1441981 ns/op        4544.79 MB/s         69349 frames/sec        105 B/op          0 allocs/op
BenchmarkMux/500-20                  164           7044468 ns/op        4651.52 MB/s         71009 frames/sec       2338 B/op         21 allocs/op
BenchmarkMux/1000-20                  85          14059162 ns/op        4661.37 MB/s         71128 frames/sec     107725 B/op        122 allocs/op
PASS
ok      github.com/Acebond/gomux        10.553s
```


## Specification

A gomux session is an exchange of *frames* between two peers over a shared connection.

### Frames

All integers in this spec are little-endian.

A frame consists of a *frame header* followed by a payload. A header is:

| Length | Type   | Description    |
|--------|--------|----------------|
|   4    | uint32 | ID             |
|   2    | uint16 | Payload length |
|   2    | uint16 | Flags          |

The ID specifies which *stream* a frame belongs to. Streams are numbered sequentially, starting at 0. To prevent collisions, streams initiated by the client peer use even IDs, while the server peer uses odd IDs.

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

The "Error" flag may only be set alongside the "Last frame" flag, and indicates that the payload contains a string describing why the stream was closed.

The "Keepalive" flag indicates a keepalive frame. Keepalives contain no payload and merely serve to keep the underlying connection open.