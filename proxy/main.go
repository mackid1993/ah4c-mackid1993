// stall_proxy — a tiny HTTP server that runs inside the ah4c container on
// 127.0.0.1 only. entrypoint.sh rewrites each ENCODER<N>_URL env var to
// point at this proxy, stashing the original under STALL_PROXY_TUNER_<N>.
//
// On each ah4c tune request we:
//   1. WriteHeader(200) immediately so ah4c's http.Get doesn't block.
//   2. WAIT tuneSettleDelay (default 2s). This gives ah4c time to run
//      prebmitune (synchronous in ah4c's tune()) and bmitune (fired as a
//      goroutine from the first reader.Read). With both scripts done,
//      the encoder is on the target channel before we pull any bytes.
//   3. Open the encoder connection and stream through a stallTolerantReader
//      for continued resilience (mid-stream encoder reboots, etc.).
//
// This replicates the effective behavior of PR #9's prebmitune-before-
// http.Get reorder without touching ah4c source. Override the delay with
// STALL_PROXY_TUNE_DELAY_MS if your scripts take longer than 2s.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	listenAddr          = "127.0.0.1:47811"
	defaultSettleDelay  = 2 * time.Second
	settleDelayEnvVar   = "STALL_PROXY_TUNE_DELAY_MS"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/tuner/", handleTuner)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Kernel default TCP buffers — with the settle delay in place there is
	// no pre-read accumulation to worry about, so a generous socket buffer
	// just gives more throughput headroom if ah4c's drain rate blips
	// (GC pauses, scheduler stalls, etc.), preventing the rare slow-motion
	// playback moment that tight buffers were causing.
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("stall_proxy listening on %s (tune settle delay %s)", listenAddr, settleDelay())
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func settleDelay() time.Duration {
	if v := os.Getenv(settleDelayEnvVar); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return defaultSettleDelay
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

	// 1. Headers out immediately — ah4c's http.Get returns, prebmitune starts.
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	// 2. Wait for ah4c's prebmitune + bmitune to settle before connecting
	// to the encoder. Without this delay the proxy pulls bytes from the
	// old channel, then the channel switches mid-stream and DVR's demuxer
	// has to re-lock PAT/PMT — which it often does by dropping audio.
	delay := settleDelay()
	log.Printf("[%s] waiting %s for ah4c tune scripts to settle", label, delay)
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		log.Printf("[%s] client disconnected during settle delay", label)
		return
	}

	// 3. Connect and stream. By now the encoder is on the target channel,
	// so bytes going to DVR are clean from the first packet.
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

	go func() {
		<-ctx.Done()
		reader.Close()
	}()

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
