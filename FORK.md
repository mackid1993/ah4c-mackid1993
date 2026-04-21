# ah4c-mackid1993 — fork overview

This fork is a thin patch overlay on [sullrich/ah4c](https://github.com/sullrich/ah4c). Upstream Go source is built **unmodified**; stall tolerance is provided by a small sidecar (in the same container) called `stall_proxy`.

## What the fork adds

- **`upstream/`** — git submodule pointing at `sullrich/ah4c`. Bumped daily by the sync workflow; bumped manually via the `Sync submodule from sullrich/ah4c` workflow.
- **`proxy/`** — a tiny HTTP server (`stall_proxy`) that listens on `127.0.0.1:47811` inside the container and wraps the encoder stream with:
  - a settle delay before the first encoder connect (matches PR #9's prebmitune-before-http.Get ordering),
  - a stall-tolerant reader that absorbs bmitune channel-switch gaps and reconnects automatically if the encoder drops,
  - NULL TS packet keepalives during any residual stall so DVR never sees a zero-byte gap.
- **`entrypoint.sh`** — launches `stall_proxy`, rewrites each `ENCODER<N>_URL` to point at the proxy (stashing the original under `STALL_PROXY_TUNER_<N>`), then execs upstream's `docker-start.sh` unchanged.

ah4c's `tune()` calls `http.Get(ENCODER<N>_URL)` as usual; the proxy is transparent.

## Environment variables

### User-facing (you set these)

| Variable | Default | Purpose |
|---|---|---|
| `STALL_PROXY_TUNE_DELAY_MS` | `2000` | Milliseconds the proxy waits after accepting a tune request before connecting to the real encoder. Must be long enough for ah4c's `prebmitune` (synchronous) plus `bmitune` (async goroutine from the first `reader.Read`) to finish. Bump this if your encoder's channel-switch script takes longer than 2 seconds. `0` disables the delay. |

All other upstream ah4c env vars (`NUMBER_TUNERS`, `ENCODER<N>_URL`, `TUNER<N>_IP`, `CMD<N>`, `TEECMD<N>`, `STREAMER_APP`, etc.) work exactly as documented in the upstream README.

### Set internally by `entrypoint.sh` (don't set these yourself)

| Variable | Purpose |
|---|---|
| `STALL_PROXY_TUNER_<N>` | For each `ENCODER<N>_URL` the entrypoint copies the original encoder URL here before rewriting `ENCODER<N>_URL` to `http://127.0.0.1:47811/tuner/<N>`. The proxy reads `STALL_PROXY_TUNER_<N>` to find the real upstream. |

## Verifying the proxy is running

```bash
# Proxy logs appear in the same container log as ah4c:
docker logs ah4c | grep -E "stall_proxy|entrypoint"

# Should see something like:
#   [entrypoint] tuner 1: http://your-encoder/stream -> http://127.0.0.1:47811/tuner/1
#   [entrypoint] stall_proxy ready on 127.0.0.1:47811
#   stall_proxy listening on 127.0.0.1:47811 (tune settle delay 2s)

# Health check:
docker exec ah4c curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:47811/healthz
# -> 200

# Confirm ENCODER<N>_URL was rewritten for ah4c:
docker exec ah4c cat /proc/1/environ | tr '\0' '\n' | grep -E "ENCODER|STALL_PROXY"
```

## GitHub Actions workflows

- **Test build** — runs on every push/PR, builds the image to verify nothing is broken.
- **Sync submodule from sullrich/ah4c** — daily schedule (13:00 UTC ≈ 9 AM Eastern) plus manual dispatch. Advances `upstream/` submodule, rebuilds to verify, opens a GitHub issue on success so you know to run the publish workflow.
- **Build and publish image** — manual dispatch. Builds `linux/amd64` and pushes to `ghcr.io/mackid1993/ah4c-mackid1993:latest` (optional extra tag input).

## Tuning tips

- If DVR shows buffering / behind-timeline on first tune: `STALL_PROXY_TUNE_DELAY_MS` may be too short for your hardware's script timing. Bump in increments of 500ms.
- If warm-tune latency feels excessive: your scripts finish fast — try `STALL_PROXY_TUNE_DELAY_MS=1000` or `500`.
- The proxy adds zero latency in steady state once streaming; the delay only applies at tune start.
