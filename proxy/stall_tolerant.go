package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// nullTSPacket — a 188-byte MPEG-TS NULL packet (PID 0x1FFF). TS demuxers
// drop these on demux, so they're safe to emit as keepalive bytes when the
// upstream encoder is cold or stalled.
var nullTSPacket = func() [188]byte {
	var p [188]byte
	p[0] = 0x47
	p[1] = 0x1F
	p[2] = 0xFF
	p[3] = 0x10
	for i := 4; i < 188; i++ {
		p[i] = 0xFF
	}
	return p
}()

const (
	stallReadGap          = 500 * time.Millisecond
	firstChunkGap         = 15 * time.Second // how long Read() waits for the first real chunk before giving up as EOF
	srcStallReconnect     = 5 * time.Second
	srcReconnectBackoff   = 2 * time.Second
	maxUnhealthyDuration  = 15 * time.Second
	chunkSize             = 32 * 1024
	// queueDepth = 0: unbuffered channel. Producer's push BLOCKS until
	// Read() pulls. There is never more than one chunk in flight between
	// the encoder body and DVR, so DVR tracks live with the minimum lag
	// the IPC pipeline physically allows (1 chunk + tuned TCP buffers).
	// Use this when even ~80ms of stable lag is visible on the DVR UI.
	queueDepth = 0
	reconnectLogThrottle  = 10 * time.Second
)

// stallTolerantReader wraps an HTTP response body so downstream consumers
// (Channels DVR) never observe a zero-byte gap when the upstream encoder is
// cold, stalled, or cycling. Warm streams pass through untouched: bytes go
// producer->queue->Read() with sub-millisecond overhead. Only when the queue
// has been empty for stallReadGap (500ms) do NULL TS packets fill in.
//
// First-chunk gate: Read() refuses to emit NULLs until the producer has
// delivered at least one real chunk. This matches the effective behavior of
// PR #9's reader in ah4c's source, where the first Read() only happened
// after tune()'s prebmitune finished — by then the producer had filled the
// queue, so the 500ms timer never fired at startup. The proxy invokes
// Read() immediately on HTTP request instead, so without this gate DVR
// would see a NULL preamble and lose audio PID lock.
type stallTolerantReader struct {
	chunks        chan []byte
	closed        chan struct{}
	closeOnce     sync.Once
	bodyMu        sync.Mutex
	body          io.ReadCloser
	reconnectFn   func() (io.ReadCloser, error)
	label         string
	hasFirstChunk atomic.Bool
}

func newStallTolerantReader(body io.ReadCloser, reconnectFn func() (io.ReadCloser, error), label string) *stallTolerantReader {
	s := &stallTolerantReader{
		chunks:      make(chan []byte, queueDepth),
		closed:      make(chan struct{}),
		body:        body,
		reconnectFn: reconnectFn,
		label:       label,
	}
	go s.producer()
	return s
}

func (s *stallTolerantReader) producer() {
	chunk := make([]byte, chunkSize)
	lastRealBytes := time.Now()
	giveUp := func(reason string) {
		log.Printf("[%s] %s; closing reader so DVR sees EOF", s.label, reason)
		s.closeOnce.Do(func() { close(s.closed) })
	}
	for {
		select {
		case <-s.closed:
			return
		default:
		}
		if time.Since(lastRealBytes) > maxUnhealthyDuration {
			giveUp(fmt.Sprintf("no source bytes for %v", maxUnhealthyDuration))
			return
		}
		s.bodyMu.Lock()
		body := s.body
		s.bodyMu.Unlock()

		var n int
		var err error
		if body == nil {
			// No upstream body yet — force an immediate reconnect attempt.
			err = fmt.Errorf("no upstream body yet")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), srcStallReconnect)
			n, err = readWithDeadline(ctx, body, chunk)
			cancel()
		}
		if n > 0 {
			lastRealBytes = time.Now()
			data := make([]byte, n)
			copy(data, chunk[:n])
			select {
			case s.chunks <- data:
			case <-s.closed:
				return
			}
			if err == nil {
				continue
			}
		}
		// n == 0 OR err != nil after partial read.
		if err != nil && body != nil {
			log.Printf("[%s] source idle/error (%v); reconnecting", s.label, err)
		}
		if body != nil {
			body.Close()
		}
		if s.reconnectFn == nil {
			s.closeOnce.Do(func() { close(s.closed) })
			return
		}
		// Try to reconnect. While this loops, DVR keeps getting NULL packets
		// (once we have delivered a first chunk; before that Read() blocks).
		var newBody io.ReadCloser
		lastReconnectLog := time.Time{}
		for {
			select {
			case <-s.closed:
				return
			default:
			}
			if time.Since(lastRealBytes) > maxUnhealthyDuration {
				giveUp(fmt.Sprintf("no source bytes for %v during reconnect", maxUnhealthyDuration))
				return
			}
			nb, rerr := s.reconnectFn()
			if rerr == nil {
				newBody = nb
				break
			}
			// Throttle log spam during long cold boots.
			if time.Since(lastReconnectLog) > reconnectLogThrottle {
				log.Printf("[%s] reconnect failed: %v", s.label, rerr)
				lastReconnectLog = time.Now()
			}
			select {
			case <-time.After(srcReconnectBackoff):
			case <-s.closed:
				return
			}
		}
		log.Printf("[%s] reconnected", s.label)
		s.bodyMu.Lock()
		s.body = newBody
		s.bodyMu.Unlock()
	}
}

func (s *stallTolerantReader) Read(p []byte) (int, error) {
	// Before the first real chunk, block without emitting NULLs so DVR's
	// demuxer sees a valid PAT/PMT at stream start and locks the audio PID.
	// If the encoder never produces within firstChunkGap we return EOF so
	// ah4c can fall through to the next tuner instead of streaming NULLs.
	if !s.hasFirstChunk.Load() {
		timer := time.NewTimer(firstChunkGap)
		defer timer.Stop()
		select {
		case <-s.closed:
			return 0, io.EOF
		case data := <-s.chunks:
			s.hasFirstChunk.Store(true)
			return copy(p, data), nil
		case <-timer.C:
			return 0, io.EOF
		}
	}

	timer := time.NewTimer(stallReadGap)
	defer timer.Stop()
	select {
	case <-s.closed:
		return 0, io.EOF
	case data := <-s.chunks:
		return copy(p, data), nil
	case <-timer.C:
		n := 0
		for n+188 <= len(p) {
			copy(p[n:n+188], nullTSPacket[:])
			n += 188
		}
		if n == 0 {
			return copy(p, nullTSPacket[:]), nil
		}
		return n, nil
	}
}

func (s *stallTolerantReader) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	s.bodyMu.Lock()
	body := s.body
	s.bodyMu.Unlock()
	if body != nil {
		return body.Close()
	}
	return nil
}

func readWithDeadline(ctx context.Context, r io.Reader, buf []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := r.Read(buf)
		ch <- result{n, err}
	}()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
