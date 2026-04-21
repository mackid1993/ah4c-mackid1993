// stall_proxy — a tiny HTTP server that runs inside the ah4c container on
// 127.0.0.1 only. entrypoint.sh rewrites each ENCODER<N>_URL env var to
// point at this proxy, stashing the original under STALL_PROXY_TUNER_<N>.
//
// On each ah4c tune request we:
//   1. WriteHeader(200) immediately so ah4c's http.Get doesn't block.
//   2. Watch /proc for the bmitune.sh process matching this tuner's IP,
//      and wait until it exits. That is the actual, authoritative signal
//      that the encoder is on the target channel — no fixed timer, no
//      guessing per-hardware. Falls back to a brief safety delay if the
//      script can't be detected (already finished, atypical setup).
//   3. Open the encoder connection and stream through a stallTolerantReader
//      for continued resilience (mid-stream encoder reboots, etc.).
//
// This replicates the effective behavior of PR #9's prebmitune-before-
// http.Get reorder without touching ah4c source, and adapts to whatever
// duration the user's bmitune.sh actually takes.
package main

import (
	"context"
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
	listenAddr            = "127.0.0.1:47811"
	defaultMaxTuneWait    = 30 * time.Second
	bmituneScanWindow     = 3 * time.Second // how long to look for bmitune to appear before giving up
	bmituneFallbackWait   = 500 * time.Millisecond
	defaultPostBmituneSlp = 1 * time.Second
	maxWaitEnvVar         = "STALL_PROXY_TUNE_DELAY_MS"       // total time budget for the bmitune wait
	postBmituneEnvVar     = "STALL_PROXY_POST_BMITUNE_MS"     // grace sleep after bmitune.sh exits
)

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
	log.Printf("stall_proxy listening on %s (max bmitune wait %s, post-bmitune sleep %s)",
		listenAddr, maxTuneWait(), postBmituneSleep())
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func maxTuneWait() time.Duration {
	if v := os.Getenv(maxWaitEnvVar); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return defaultMaxTuneWait
}

func postBmituneSleep() time.Duration {
	if v := os.Getenv(postBmituneEnvVar); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return defaultPostBmituneSlp
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

	// 2. Wait for bmitune.sh (this tuner's channel-change script) to finish.
	waitForBmituneExit(ctx, n, label, maxTuneWait())

	// 2b. Post-bmitune grace sleep. Many bmitune scripts dispatch an
	// asynchronous command (e.g. ADB on an Osprey) and exit immediately;
	// the actual encoder channel switch can complete several seconds
	// after bmitune.sh returns. Sleeping briefly before connecting keeps
	// DVR from receiving old-channel bytes that then EOF and force a
	// PAT/PMT re-lock. Tune with STALL_PROXY_POST_BMITUNE_MS.
	postSleep := postBmituneSleep()
	if postSleep > 0 {
		log.Printf("[%s] post-bmitune sleep %s before encoder connect", label, postSleep)
		sleepWithCtx(ctx, postSleep)
	}

	// 3. Connect and stream. Encoder is now on the target channel.
	log.Printf("[%s] connecting to encoder %s", label, upstream)
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

// waitForBmituneExit polls /proc for bmitune.sh matching TUNER<N>_IP and
// blocks until that process exits or the total budget elapses. If the
// script can't be located within bmituneScanWindow the function falls
// back to a brief sleep — bmitune may have run too fast to catch, or the
// env vars needed to identify it aren't set.
func waitForBmituneExit(ctx context.Context, tunerN, label string, budget time.Duration) {
	tunerIP := os.Getenv("TUNER" + tunerN + "_IP")
	streamerApp := os.Getenv("STREAMER_APP")
	if tunerIP == "" || streamerApp == "" {
		log.Printf("[%s] TUNER%s_IP or STREAMER_APP unset — %s safety delay", label, tunerN, bmituneFallbackWait)
		sleepWithCtx(ctx, bmituneFallbackWait)
		return
	}
	needleScript := streamerApp + "/bmitune.sh"
	needleIP := tunerIP

	deadline := time.Now().Add(budget)
	scanDeadline := time.Now().Add(bmituneScanWindow)
	if scanDeadline.After(deadline) {
		scanDeadline = deadline
	}

	// Phase 1: wait for bmitune.sh to appear in /proc.
	var pid int
	for time.Now().Before(scanDeadline) {
		if ctx.Err() != nil {
			return
		}
		pid = findBmituneProcess(needleScript, needleIP)
		if pid > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid == 0 {
		log.Printf("[%s] bmitune.sh not detected within %s — %s safety delay", label, bmituneScanWindow, bmituneFallbackWait)
		sleepWithCtx(ctx, bmituneFallbackWait)
		return
	}
	log.Printf("[%s] bmitune.sh pid=%d running; waiting for exit", label, pid)

	// Phase 2: wait for that PID to exit.
	start := time.Now()
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		if !processExists(pid) {
			log.Printf("[%s] bmitune.sh exited after %s", label, time.Since(start).Round(time.Millisecond))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("[%s] bmitune.sh still running after %s budget — proceeding anyway", label, budget)
}

func findBmituneProcess(needleScript, needleIP string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		cmd := string(data)
		if strings.Contains(cmd, needleScript) && strings.Contains(cmd, needleIP) {
			return pid
		}
	}
	return 0
}

func processExists(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

func sleepWithCtx(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}
