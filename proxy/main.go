// stall_proxy — a tiny HTTP server that runs inside the ah4c container on
// 127.0.0.1 only. entrypoint.sh rewrites each ENCODER<N>_URL env var to
// point at this proxy, stashing the original under STALL_PROXY_TUNER_<N>.
//
// When ah4c does http.Get on the proxy URL, we immediately return 200 and
// a streaming body backed by a stallTolerantReader. The reader emits real
// encoder bytes once the upstream is up, and MPEG-TS NULL packets during
// any cold-boot or stall window. ah4c never sees "connection refused"
// on a cold encoder — the tune just looks like a stream that starts quiet.
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
		// No write timeout — streams can be long-lived.
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

	// Close the reader when the client (ah4c) goes away.
	ctx := r.Context()

	// Respond 200 + headers before the upstream is even contacted, so ah4c
	// sees an immediately-successful tune regardless of encoder warmth.
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	log.Printf("[%s] client connected; upstream=%s", label, upstream)

	reader := newStallTolerantReader(nil, func() (io.ReadCloser, error) {
		resp, err := http.Get(upstream)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("upstream status %s", resp.Status)
		}
		return resp.Body, nil
	}, label)
	defer reader.Close()

	// Tie reader lifetime to client disconnect.
	go func() {
		<-ctx.Done()
		reader.Close()
	}()

	if _, err := io.Copy(w, reader); err != nil {
		log.Printf("[%s] client stream ended: %v", label, err)
	} else {
		log.Printf("[%s] client stream ended cleanly", label)
	}
}
