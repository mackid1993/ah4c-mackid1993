#!/bin/bash
# Container entrypoint. Launches stall_proxy on 127.0.0.1:47811, rewrites
# ENCODER<N>_URL env vars so ah4c talks to the proxy instead of the encoder,
# then supervises both stall_proxy and upstream's docker-start.sh. If either
# child dies, the whole container exits non-zero so Docker's restart policy
# brings it back (set `restart: unless-stopped` in your compose file).
set -euo pipefail

PROXY_PORT=47811
PROXY_HOST=127.0.0.1

# --- env-var rewrite ----------------------------------------------------
# Copy each ENCODER<N>_URL into STALL_PROXY_TUNER_<N> and rewrite the
# ENCODER<N>_URL to point at the proxy. ah4c reads ENCODER<N>_URL unchanged;
# the proxy reads STALL_PROXY_TUNER_<N> to find the real upstream.
num="${NUMBER_TUNERS:-0}"
if [[ "$num" -gt 0 ]]; then
  for i in $(seq 1 "$num"); do
    var="ENCODER${i}_URL"
    orig="${!var:-}"
    if [[ -n "$orig" ]]; then
      export "STALL_PROXY_TUNER_${i}=${orig}"
      export "${var}=http://${PROXY_HOST}:${PROXY_PORT}/tuner/${i}"
      echo "[entrypoint] tuner ${i}: ${orig} -> ${!var}"
    fi
  done
fi

# --- launch stall_proxy -------------------------------------------------
/opt/stall_proxy &
PROXY_PID=$!

# Wait for proxy to be listening (pure bash, no netcat dependency).
for _ in $(seq 1 100); do
  if exec 3<>/dev/tcp/${PROXY_HOST}/${PROXY_PORT} 2>/dev/null; then
    exec 3<&-; exec 3>&-
    break
  fi
  if ! kill -0 "$PROXY_PID" 2>/dev/null; then
    echo "[entrypoint] stall_proxy died before accepting connections" >&2
    exit 1
  fi
  sleep 0.1
done

echo "[entrypoint] stall_proxy ready on ${PROXY_HOST}:${PROXY_PORT}"

# --- launch ah4c via upstream's docker-start.sh -------------------------
/opt/docker-start.sh "$@" &
DOCKER_PID=$!

# Forward SIGTERM/SIGINT from Docker to both children for clean shutdown.
shutdown() {
  echo "[entrypoint] signal received, shutting down children"
  kill -TERM "$PROXY_PID" "$DOCKER_PID" 2>/dev/null || true
  wait "$DOCKER_PID" 2>/dev/null || true
  wait "$PROXY_PID" 2>/dev/null || true
  exit 0
}
trap shutdown TERM INT

# --- supervise ----------------------------------------------------------
# If either child dies unexpectedly, take the whole container down so
# Docker's restart policy gives us a fresh, healthy state.
while true; do
  if ! kill -0 "$PROXY_PID" 2>/dev/null; then
    echo "[entrypoint] stall_proxy died unexpectedly — exiting so container restarts" >&2
    kill -TERM "$DOCKER_PID" 2>/dev/null || true
    wait "$DOCKER_PID" 2>/dev/null || true
    exit 1
  fi
  if ! kill -0 "$DOCKER_PID" 2>/dev/null; then
    wait "$DOCKER_PID" || docker_exit=$?
    docker_exit="${docker_exit:-0}"
    echo "[entrypoint] docker-start.sh exited with $docker_exit — shutting down proxy" >&2
    kill -TERM "$PROXY_PID" 2>/dev/null || true
    wait "$PROXY_PID" 2>/dev/null || true
    exit "$docker_exit"
  fi
  sleep 2
done
