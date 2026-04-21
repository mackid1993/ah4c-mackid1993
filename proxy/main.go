// stall_proxy — a tiny HTTP server that runs inside the ah4c container on
// 127.0.0.1 only. entrypoint.sh rewrites each ENCODER<N>_URL env var to
// point at this proxy, stashing the original under STALL_PROXY_TUNER_<N>.
//
// On each client (ah4c) request, the proxy opens an http.Get to the real
// encoder *first* — identical to ah4c's own tune() in PR #9. If that call
// fails, we return 5xx so ah4c falls through to the next tuner exactly
// like it would without the proxy. On success, we wrap the encoder body
// with a stallTolerantReader and stream it to ah4c.
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

	// Pre-connect to the real encoder, matching ah4c's own tune() flow.
	// If this fails, return 5xx so ah4c falls through to the next tuner.
	resp, err := http.Get(upstream)
	if err != nil {
		log.Printf("[%s] upstream http.Get failed: %v", label, err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		log.Printf("[%s] upstream status %s", label, resp.Status)
		http.Error(w, "upstream status "+resp.Status, http.StatusBadGateway)
		return
	}
	log.Printf("[%s] client connected; upstream=%s", label, upstream)

	reader := newStallTolerantReader(resp.Body, func() (io.ReadCloser, error) {
		r, e := http.Get(upstream)
		if e != nil {
			return nil, e
		}
		if r.StatusCode != http.StatusOK {
			r.Body.Close()
			return nil, fmt.Errorf("status %s", r.Status)
		}
		return r.Body, nil
	}, label)
	defer reader.Close()

	// Stop the reader when ah4c disconnects.
	ctx := r.Context()
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
		log.Printf("[%s] client stream ended: %v", label, err)
	}
}
