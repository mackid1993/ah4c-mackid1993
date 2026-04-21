#!/bin/bash
# Container entrypoint. Launches stall_proxy on 127.0.0.1:47811, rewrites
# ENCODER<N>_URL env vars so ah4c talks to the proxy instead of the encoder,
# then execs the upstream docker-start.sh. Upstream source is unmodified.
set -euo pipefail

PROXY_PORT=47811
PROXY_HOST=127.0.0.1

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

# Launch proxy; it inherits the STALL_PROXY_TUNER_* env vars.
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

# Hand off to upstream's entrypoint, unmodified.
exec /opt/docker-start.sh "$@"
