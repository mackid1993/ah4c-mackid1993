#docker buildx build --platform linux/amd64 -f Dockerfile -t ghcr.io/mackid1993/ah4c-mackid1993:latest . --push

# Fork layout:
#   upstream/        git submodule -> sullrich/ah4c (Go source, scripts, assets)
#   proxy/           our stall_proxy: tiny localhost HTTP server that wraps
#                    encoder streams with stall/NULL-packet tolerance
#   entrypoint.sh    our entrypoint: launches stall_proxy, rewrites env so
#                    ah4c talks to the proxy, then execs upstream docker-start.sh
#
# ah4c source is built from upstream unmodified. No patch, no sed, no grep
# guard. Upstream can refactor tune() however they like — the stall tolerance
# lives entirely in our separate binary on 127.0.0.1.

# First Stage: Build ws-scrcpy, ah4c (unmodified), and stall_proxy
FROM golang:bookworm AS builder

ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    git nodejs npm python3 make g++ \
    && rm -rf /var/lib/apt/lists/*

# Build ws-scrcpy application
WORKDIR /ws-scrcpy
RUN git clone https://github.com/NetrisTV/ws-scrcpy.git . \
    && npm install && npm run dist

WORKDIR /ws-scrcpy/dist
RUN npm install

# Build ah4c from the pinned upstream submodule, unmodified
WORKDIR /go/src/github.com/sullrich
COPY upstream/ ./
RUN go build -o /opt/ah4c

# Build stall_proxy
WORKDIR /go/src/stall_proxy
COPY proxy/ ./
RUN go build -o /opt/stall_proxy .

# Second Stage: Runtime
FROM debian:bookworm-slim AS runner
LABEL maintainer="The Slayer <slayer@technologydragonslayer.com>"

ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive

RUN sed -i 's/^Components: .*/Components: main contrib non-free non-free-firmware/' /etc/apt/sources.list.d/debian.sources

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl bash dnsutils procps nano tzdata jq bc \
    android-tools-adb tesseract-ocr \
    nodejs npm \
    ffmpeg libva2 libva-drm2 vainfo \
    && rm -rf /var/lib/apt/lists/*

RUN if [ "$TARGETARCH" = "amd64" ]; then \
      apt-get update && apt-get install -y --no-install-recommends \
        intel-media-va-driver-non-free libmfx1 && \
      rm -rf /var/lib/apt/lists/* ; \
    fi

ENV LIBVA_DRIVER_NAME=iHD

RUN mkdir -p /opt/scripts /tmp/scripts /tmp/m3u /opt/html /opt/static

WORKDIR /opt

# Binaries
COPY --from=builder /ws-scrcpy/dist /opt/ws-scrcpy
COPY --from=builder /opt/ah4c /opt/ah4c
COPY --from=builder /opt/stall_proxy /opt/stall_proxy

# Upstream runtime assets (scripts, html, static, etc.) — content the user wants synced
COPY upstream/docker-start.sh upstream/adbpackages.sh /opt/
COPY upstream/scripts/ /tmp/scripts/
COPY upstream/m3u/ /tmp/m3u/
COPY upstream/html/ /opt/html/
RUN sed -i '/href="\/config"/d; /href="\/env"/d' /opt/html/index.html
COPY upstream/static/ /opt/static/

# Our entrypoint, which wraps upstream's docker-start.sh
COPY entrypoint.sh /opt/entrypoint.sh

RUN chmod +x /opt/docker-start.sh /opt/entrypoint.sh /opt/stall_proxy \
    && groupadd render || true

EXPOSE 7654 8000

CMD ["/opt/entrypoint.sh"]
