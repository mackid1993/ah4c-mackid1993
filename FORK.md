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
| `STALL_PROXY_TUNE_DELAY_MS` | `30000` | Maximum milliseconds the proxy will wait for `bmitune.sh` (your channel-switch script) to exit before connecting to the encoder. The proxy watches `/proc` for a `bmitune.sh` process matching this tuner's `TUNER<N>_IP` and connects the moment that process exits — no wasted time on warm tunes, no reconnect+relock drama on slow ones. You rarely need to touch this; 30s is a ceiling, not a fixed delay. |

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

## Process supervision

`entrypoint.sh` supervises both `stall_proxy` and upstream's `docker-start.sh`. If either dies, the whole container exits non-zero so Docker's restart policy brings it back in a clean state. Set `restart: unless-stopped` (or equivalent) on your container so this recovery is automatic:

```yaml
# docker-compose.yml
services:
  ah4c:
    image: ghcr.io/mackid1993/ah4c-mackid1993:latest
    restart: unless-stopped
    # ...
```

SIGTERM / SIGINT from Docker are forwarded to both children for clean shutdown.

## Tuning tips

- The proxy watches `bmitune.sh`'s process lifetime via `/proc` and connects the moment the script exits. Fast tunes finish fast, slow tunes wait exactly as long as they need — no manual tuning required.
- If the proxy logs `bmitune.sh not detected within 3s` the script ran too fast to catch, or `STREAMER_APP` / `TUNER<N>_IP` aren't in the proxy's environment. A 500ms safety delay is applied in that case. Verify those env vars are set if you see it consistently.
- `STALL_PROXY_TUNE_DELAY_MS` is a ceiling, not a fixed delay. If bmitune legitimately takes longer than 30s, raise it.
- The proxy adds zero latency in steady state once streaming; the wait only happens at tune start.
