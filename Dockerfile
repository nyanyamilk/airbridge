# Build cliraop from libraop
FROM debian:bookworm-slim AS cliraop-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    cmake \
    git \
    libssl-dev \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN git clone https://github.com/philippe44/libraop.git . \
    && git submodule update --init --recursive

WORKDIR /src/build
RUN cmake .. && make

# Build airbridge
FROM golang:1.24-bookworm AS go-builder

ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X github.com/kenyonj/airbridge/internal/web.Version=${VERSION}" -o /airbridge ./cmd/airbridge

# Final image
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libssl3 \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy binaries (cliraop binary name includes platform suffix)
COPY --from=cliraop-builder /src/build/raop_play-linux-* /app/bin/cliraop
COPY --from=go-builder /airbridge /app/airbridge

# AirPlay 2 sink. HomePod OS 27 (and tvOS 27) silently drop third-party
# AirPlay 1 audio; the native AirPlay 2 flow still works. Set
# AIRBRIDGE_SINK=ap2 (or =auto for HomePods) to use it.
ARG CLIAPLAY_VERSION=v0.5.8
ARG TARGETARCH=amd64
ADD https://github.com/music-assistant/airplay-cli/releases/download/${CLIAPLAY_VERSION}/cliairplay-linux-x86_64 /app/bin/cliairplay
RUN chmod +x /app/bin/cliairplay

# Create data directory for SQLite database
RUN mkdir -p /data

# Default environment
ENV AIRBRIDGE_DB=/data/airbridge.db
ENV AIRBRIDGE_PORT=8200
# raop (default) | ap2 | auto
ENV AIRBRIDGE_SINK=raop

EXPOSE 8200 8201

# Use host network mode for mDNS/SSDP discovery
# docker run --network=host ...

ENTRYPOINT ["/app/airbridge"]
CMD ["--web"]
