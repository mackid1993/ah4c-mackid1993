// Fallback patch for mackid1993's PR #9 on sullrich/ah4c.
// Injected into the upstream source at Docker build time via the Dockerfile;
// no edits to main.go live in this fork. See README / Dockerfile for details.

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// WrapEncoderBody wraps the encoder HTTP response body with a stall-tolerant
// reader. This is the single integration point: the Dockerfile seds
// `ReadCloser: resp.Body,` in main.go's tune() to call this.
func WrapEncoderBody(body io.ReadCloser, encoderURL, tunerip string) io.ReadCloser {
	label := fmt.Sprintf("tuner=%s", tunerip)
	return newStallTolerantReader(body, func() (io.ReadCloser, error) {
		r, e := http.Get(encoderURL)
		if e != nil {
			return nil, e
		}
		if r.StatusCode != 200 {
			r.Body.Close()
			return nil, fmt.Errorf("status %s", r.Status)
		}
		return r.Body, nil
	}, label)
}

// nullTSPacket is a single 188-byte MPEG-TS NULL packet (PID 0x1FFF). TS
// demuxers including Channels DVR drop these on demux, so they're safe to
// inject as a keepalive when the upstream encoder briefly stops producing.
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

// stallTolerantReader wraps an HTTP response body so that downstream consumers
// (Channels DVR) never observe a zero-byte gap when the upstream encoder
// stalls (e.g. while bmitune.sh triggers a channel change) or when the
// underlying TCP connection dies and needs to be re-established.
type stallTolerantReader struct {
	chunks      chan []byte
	closed      chan struct{}
	closeOnce   sync.Once
	bodyMu      sync.Mutex
	body        io.ReadCloser
	reconnectFn func() (io.ReadCloser, error)
	label       string
}

const (
	stallReadGap         = 500 * time.Millisecond
	srcStallReconnect    = 5 * time.Second
	srcReconnectBackoff  = 2 * time.Second
	maxUnhealthyDuration = 15 * time.Second
	chunkSize            = 32 * 1024
	queueDepth           = 64
)

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
		logger("[%s] %s; closing reader so DVR sees EOF", s.label, reason)
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
		ctx, cancel := context.WithTimeout(context.Background(), srcStallReconnect)
		n, err := readWithDeadline(ctx, body, chunk)
		cancel()
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
		if err != nil {
			logger("[%s] source idle/error (%v); reconnecting", s.label, err)
		}
		body.Close()
		if s.reconnectFn == nil {
			s.closeOnce.Do(func() { close(s.closed) })
			return
		}
		var newBody io.ReadCloser
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
			logger("[%s] reconnect failed: %v", s.label, rerr)
			select {
			case <-time.After(srcReconnectBackoff):
			case <-s.closed:
				return
			}
		}
		logger("[%s] reconnected", s.label)
		s.bodyMu.Lock()
		s.body = newBody
		s.bodyMu.Unlock()
	}
}

func (s *stallTolerantReader) Read(p []byte) (int, error) {
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
