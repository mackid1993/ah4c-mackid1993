// stall_proxy — a tiny HTTP server that runs inside the ah4c container on
// 127.0.0.1 only. entrypoint.sh rewrites each ENCODER<N>_URL env var to
// point at this proxy, stashing the original under STALL_PROXY_TUNER_<N>.
//
// On each client (ah4c) request we respond 200 immediately and hand back a
// stallTolerantReader-backed streaming body. The reader's producer opens
// the real encoder connection in the background; Read() holds until the
// first real chunk arrives so DVR never sees a NULL-only preamble.
//
// This differs architecturally from PR #9's inline wrapper (ah4c's
// http.Get blocks on the encoder itself) but preserves the observable
// behavior: warm tunes start fast, cold/stalled tunes see NULL packets
// only AFTER a real stream has begun, and ah4c's http.Get never hangs
// past ah4c's own timeouts because we return 200 right away.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const listenAddr = "127.0.0.1:47811"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/tuner/", handleTuner)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// Shrink the kernel TCP send buffer on each accepted connection
		// (proxy -> ah4c loopback). Linux default is ~87KB, which holds
		// ~140ms of a 5 Mbps stream — enough to keep DVR visibly behind
		// live TV regardless of how tight our user-space buffering is.
		// 16KB caps that to ~25ms; loopback has zero RTT so throughput
		// is not impacted.
		ConnState: func(c net.Conn, state http.ConnState) {
			if state != http.StateNew {
				return
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetWriteBuffer(16 * 1024)
				_ = tc.SetReadBuffer(16 * 1024)
			}
		},
	}
	log.Printf("stall_proxy listening on %s", listenAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func handleTuner(w http.ResponseWriter, r *http.Request) {
	n := strings.TrimPrefix(r.URL.Path, "/tuner/")
	if n == "" || strings.ContainsAny(n, "/?") {
		http.Error(w, "bad tuner id", http.StatusBadRequest)
		return
	}
	upstream := os.Getenv("STALL_PROXY_TUNER_" + n)
	if upstream == "" {
		http.Error(w, "unknown tuner "+n, http.StatusNotFound)
		return
	}
	label := "tuner=" + n
	ctx := r.Context()

	log.Printf("[%s] client connected; upstream=%s", label, upstream)

	reader := newStallTolerantReader(nil, func() (io.ReadCloser, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("status %s", resp.Status)
		}
		return resp.Body, nil
	}, label)
	defer reader.Close()

	// Stop the reader when ah4c disconnects.
	go func() {
		<-ctx.Done()
		reader.Close()
	}()

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Custom copy loop with flush-after-write so bytes leave the proxy the
	// instant they arrive from the producer. io.Copy by itself would sit on
	// Go's default HTTP response bufio (~4KB), adding avoidable latency.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				log.Printf("[%s] client write failed: %v", label, werr)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				log.Printf("[%s] stream ended: %v", label, rerr)
			}
			return
		}
	}
}
