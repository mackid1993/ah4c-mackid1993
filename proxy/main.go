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

	if _, err := io.Copy(w, reader); err != nil {
		log.Printf("[%s] stream ended: %v", label, err)
	}
}
