package main

import (
	"context"
	"errors"
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
	stallReadGap        = 500 * time.Millisecond
	firstChunkGap       = 15 * time.Second // how long Read() waits for the first real chunk before giving up as EOF
	srcStallReconnect   = 5 * time.Second
	srcReconnectBackoff = 2 * time.Second
	// Two unhealthy-duration budgets:
	//   - preFirstChunk: how long we'll try before ah4c gives up on this
	//     tuner and falls through. Short; matches PR #9 tune-failover policy.
	//   - postFirstChunk: how long we tolerate a mid-stream glitch on an
	//     ALREADY-PLAYING recording before closing the stream. Generous,
	//     because an encoder hiccup or source-app re-launch in the middle
	//     of a 2-hour recording should not end the recording.
	preFirstChunkUnhealthy  = 15 * time.Second
	postFirstChunkUnhealthy = 3 * time.Minute
	chunkSize               = 32 * 1024
	// queueDepth = 64: matches PR #9 exactly. At ~5 Mbps this is ~3s of
	// buffered bytes — big enough to silently absorb a typical bmitune
	// channel-switch stall without the 500ms NULL timer firing, which is
	// what keeps DVR's audio PID lock intact. In steady state the queue
	// only fills to prebmitune-duration-worth of chunks (LinkPi is fast,
	// so real in-flight lag is bounded to a few hundred ms, not the full
	// 3s theoretical ceiling).
	queueDepth = 64
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

// unhealthyLimit returns the tolerated no-bytes duration — short before
// the first real chunk (so a dead tuner fails over quickly), generous
// after (so a mid-stream glitch on a long recording doesn't end it).
func (s *stallTolerantReader) unhealthyLimit() time.Duration {
	if s.hasFirstChunk.Load() {
		return postFirstChunkUnhealthy
	}
	return preFirstChunkUnhealthy
}

func (s *stallTolerantReader) producer() {
	chunk := make([]byte, chunkSize)
	lastRealBytes := time.Now()
	haveConnectedOnce := false
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
		if time.Since(lastRealBytes) > s.unhealthyLimit() {
			giveUp(fmt.Sprintf("no source bytes for %v", s.unhealthyLimit()))
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
		// n == 0 OR err != nil after partial read. Treat context.Canceled
		// as a shutdown signal from the client (DVR/ah4c disconnected):
		// close the reader and return without logging a misleading
		// "reconnecting" message. Same for an already-set s.closed.
		if errors.Is(err, context.Canceled) {
			if body != nil {
				body.Close()
			}
			s.closeOnce.Do(func() { close(s.closed) })
			return
		}
		select {
		case <-s.closed:
			return
		default:
		}
		if err != nil && body != nil {
			log.Printf("[%s] lost encoder stream (%v); reconnecting", s.label, err)
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
			if time.Since(lastRealBytes) > s.unhealthyLimit() {
				giveUp(fmt.Sprintf("no source bytes for %v during reconnect", s.unhealthyLimit()))
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
		if haveConnectedOnce {
			log.Printf("[%s] reconnected to encoder", s.label)
		} else {
			log.Printf("[%s] connected to encoder", s.label)
			haveConnectedOnce = true
		}
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
	// Goroutine uses its own buffer so if the deadline fires before the
	// Read returns, the leaked goroutine can't race on the caller's
	// buffer (which the producer reuses across iterations).
	innerBuf := make([]byte, len(buf))
	go func() {
		n, err := r.Read(innerBuf)
		ch <- result{n, err}
	}()
	select {
	case res := <-ch:
		copy(buf, innerBuf[:res.n])
		return res.n, res.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
