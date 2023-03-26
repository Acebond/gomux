package gomux

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"testing"
	"time"
)

func newTestingPair(tb testing.TB) (dialed, accepted *Mux) {
	errChanServer := make(chan error, 1)
	errChanClient := make(chan error, 1)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		l, err := net.Listen("tcp", "127.0.0.1:60000")
		if err != nil {
			errChanServer <- err
			return
		}
		defer l.Close()
		conn, err := l.Accept()
		if err != nil {
			errChanServer <- err
			return
		}
		accepted = Server(conn, "test")
		close(errChanServer)
		wg.Done()
	}()

	go func() {
		conn, err := net.Dial("tcp", "127.0.0.1:60000")
		if err != nil {
			errChanClient <- err
			return
		}
		dialed = Client(conn, "test")
		close(errChanClient)
		wg.Done()
	}()

	if err := <-errChanServer; err != nil {
		tb.Fatal(err)
	}
	if err := <-errChanClient; err != nil {
		tb.Fatal(err)
	}
	wg.Wait()
	return
}

func handleStreams(m *Mux, fn func(*Stream) error) chan error {
	errChan := make(chan error, 1)
	go func() {
		for {
			s, err := m.AcceptStream()
			if err != nil {
				errChan <- err
				return
			}
			go func() {
				defer s.Close()
				if err := fn(s); err != nil {
					errChan <- err
					return
				}
			}()
		}
	}()
	return errChan
}

func TestMux(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:60000")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	serverCh := make(chan error, 1)
	go func() {
		serverCh <- func() error {
			conn, err := l.Accept()
			if err != nil {
				return err
			}
			m := Server(conn, "test")
			defer m.Close()
			s, err := m.AcceptStream()
			if err != nil {
				return err
			}
			defer s.Close()
			buf := make([]byte, 100)
			if n, err := s.Read(buf); err != nil {
				return err
			} else if _, err := fmt.Fprintf(s, "hello, %s!", buf[:n]); err != nil {
				return err
			}
			wg.Done()
			wg.Wait()
			return nil
		}()
	}()

	conn, err := net.Dial("tcp", "127.0.0.1:60000")
	if err != nil {
		t.Fatal(err)
	}
	m := Client(conn, "test")
	defer m.Close()
	s, err := m.OpenStream()
	if err != nil {
		t.Fatal(err.Error())
	}
	defer s.Close()
	buf := make([]byte, 100)
	if _, err := s.Write([]byte("world")); err != nil {
		t.Fatal(err)
	} else if n, err := io.ReadFull(s, buf[:13]); err != nil {
		t.Fatal(err)
	} else if string(buf[:n]) != "hello, world!" {
		t.Fatal("bad hello:", string(buf[:n]))
	}
	if err := s.Close(); err != nil && err != ErrPeerClosedConn {
		t.Fatal(err)
	}

	wg.Done()

	if err := <-serverCh; err != nil && err != ErrPeerClosedStream {
		t.Fatal(err)
	}
	// all streams should have been deleted
	time.Sleep(time.Millisecond * 100)
	m.readMutex.Lock()
	defer m.readMutex.Unlock()
	if len(m.streams) != 0 {
		t.Error("streams not closed")
	}
}

func TestManyStreams(t *testing.T) {
	m1, m2 := newTestingPair(t)

	serverCh := handleStreams(m2, func(s *Stream) error {
		// simple echo handler
		buf := make([]byte, 100)
		n, _ := s.Read(buf)
		s.Write(buf[:n])
		return nil
	})

	// spawn 100 streams
	var wg sync.WaitGroup
	errChan := make(chan error, 100)
	for i := 0; i < cap(errChan); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := m1.OpenStream()
			if err != nil {
				t.Fatal(err.Error())
			}
			defer s.Close()
			msg := fmt.Sprintf("hello, %v!", i)
			buf := make([]byte, len(msg))
			if _, err := s.Write([]byte(msg)); err != nil {
				errChan <- err
			} else if _, err := io.ReadFull(s, buf); err != nil {
				errChan <- err
			} else if string(buf) != msg {
				errChan <- err
			} else if err := s.Close(); err != nil {
				errChan <- err
			}
		}(i)
	}
	wg.Wait()
	close(errChan)
	for err := range errChan {
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := m1.Close(); err != nil {
		t.Fatal(err)
	} else if err := <-serverCh; err != nil && err != ErrPeerClosedConn {
		t.Fatal(err)
	}

	// all streams should have been deleted
	time.Sleep(time.Millisecond * 100)
	m1.readMutex.Lock()
	defer m1.readMutex.Unlock()
	if len(m1.streams) != 0 {
		t.Error("streams not closed:", len(m1.streams))
	}
}

func TestDeadline(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	m1, m2 := newTestingPair(t)

	serverCh := handleStreams(m2, func(s *Stream) error {
		// wait 100ms before reading/writing
		buf := make([]byte, 100)
		time.Sleep(100 * time.Millisecond)
		if _, err := s.Read(buf); err != nil && err != io.EOF {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		if _, err := s.Write([]byte("hello, world!")); err != nil {
			return err
		} else if err := s.Close(); err != nil {
			return err
		}
		return nil
	})

	// a Read deadline should not timeout a Write
	s, err := m1.OpenStream()
	if err != nil {
		t.Fatal("OpenStream() failed")
	}
	buf := []byte("hello, world!")
	s.SetReadDeadline(time.Now().Add(time.Millisecond))
	time.Sleep(2 * time.Millisecond)
	_, err = s.Write(buf)
	s.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatal("SetReadDeadline caused Write to fail:", err)
	} else if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatal(err)
	} else if string(buf) != "hello, world!" {
		t.Fatal("bad echo")
	} else if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		timeout bool
		fn      func(*Stream)
	}{
		{false, func(*Stream) {}}, // no deadline
		{false, func(s *Stream) {
			s.SetDeadline(time.Now().Add(time.Hour)) // plenty of time
		}},
		{true, func(s *Stream) {
			s.SetDeadline(time.Now().Add(time.Millisecond)) // too short
		}},
		{true, func(s *Stream) {
			s.SetDeadline(time.Now().Add(time.Nanosecond))
			s.SetReadDeadline(time.Time{}) // Write should still fail
		}},
		{true, func(s *Stream) {
			s.SetDeadline(time.Now().Add(time.Millisecond))
			s.SetWriteDeadline(time.Time{}) // Read should still fail
		}},
		{false, func(s *Stream) {
			s.SetDeadline(time.Now())
			s.SetDeadline(time.Time{}) // should overwrite
		}},
		{false, func(s *Stream) {
			s.SetDeadline(time.Now().Add(time.Millisecond))
			s.SetWriteDeadline(time.Time{}) // overwrites Read
			s.SetReadDeadline(time.Time{})  // overwrites Write
		}},
	}
	for i, test := range tests {
		err := func() error {
			s, err := m1.OpenStream()
			if err != nil {
				panic(err)
			}
			defer s.Close()
			test.fn(s) // set deadlines
			time.Sleep(time.Millisecond)
			// need to write a fairly large message; otherwise the packets just
			// get buffered and "succeed" instantly
			//time.Sleep(time.Millisecond)
			if _, err := s.Write(make([]byte, math.MaxUint16/2)); err != nil {
				return fmt.Errorf("foo: %w", err)
			} else if _, err := io.ReadFull(s, buf[:13]); err != nil {
				return err
			} else if string(buf) != "hello, world!" {
				return errors.New("bad echo")
			}
			return s.Close()
		}()
		if isTimeout := errors.Is(err, os.ErrDeadlineExceeded); test.timeout != isTimeout {
			t.Errorf("test %v: expected timeout=%v, got %v", i, test.timeout, err)
		}
	}

	if err := m1.Close(); err != nil {
		t.Fatal(err)
	} else if err := <-serverCh; err != nil && err != ErrPeerClosedConn && err != ErrPeerClosedStream {
		t.Fatal(err)
	}
}

func BenchmarkMux(b *testing.B) {
	debug.SetGCPercent(-1)
	numStreams := 500

	m1, m2 := newTestingPair(b)
	defer m2.Close()
	defer m1.Close()

	_ = handleStreams(m2, func(s *Stream) error {
		io.Copy(io.Discard, s)
		return nil
	})

	// open each stream in a separate goroutine
	bufSize := math.MaxUint16
	buf := make([]byte, bufSize)
	b.ResetTimer()
	b.SetBytes(int64(bufSize * numStreams))
	b.ReportAllocs()
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(numStreams)
	for j := 0; j < numStreams; j++ {
		go func() {
			defer wg.Done()
			s, err := m1.OpenStream()
			if err != nil {
				panic(err)
			}
			defer s.Close()
			for i := 0; i < b.N; i++ {
				if _, err := s.Write(buf); err != nil {
					panic(err)
				}
			}
		}()
	}
	wg.Wait()
	b.ReportMetric(float64(b.N*numStreams)/time.Since(start).Seconds(), "frames/sec")

	//f, _ := os.Create("heap.out")
	//pprof.WriteHeapProfile(f)
	//f.Close()
}

// benchmark throughput of raw TCP conn
func BenchmarkConn(b *testing.B) {
	l, err := net.Listen("tcp", "127.0.0.1:60000")
	if err != nil {
		b.Fatal(err)
	}
	serverCh := make(chan error, 1)
	go func() {
		serverCh <- func() error {
			conn, err := l.Accept()
			if err != nil {
				return err
			}
			defer conn.Close()

			buf := make([]byte, math.MaxUint16)
			for {
				_, err := io.ReadFull(conn, buf)
				if err != nil {
					return err
				}
			}
		}()
	}()
	defer func() {
		if err := <-serverCh; err != nil && err != io.EOF {
			b.Fatal(err)
		}
	}()

	conn, err := net.Dial("tcp", "127.0.0.1:60000")
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()

	buf := make([]byte, math.MaxUint16)
	b.ResetTimer()
	b.SetBytes(math.MaxUint16)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := conn.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
}
