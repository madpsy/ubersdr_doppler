# syntax=docker/dockerfile:1
# ---------------------------------------------------------------------------
# Stage 1: build ubersdr_doppler Go binary
# ---------------------------------------------------------------------------
FROM golang:1.24-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/ubersdr_doppler ./...

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -s /bin/false doppler

COPY --from=go-builder /out/ubersdr_doppler /usr/local/bin/ubersdr_doppler

# Copy entrypoint script (translates env vars to ubersdr_doppler flags)
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# Create the default data directory and ensure the doppler user owns it.
# Note: no VOLUME declaration — the docker-compose.yml bind mount handles persistence.
# A VOLUME declaration would cause Docker to create a root-owned anonymous volume
# that overwrites the chown, preventing the doppler user from writing to /data.
RUN chmod +x /usr/local/bin/entrypoint.sh \
    && mkdir -p /data \
    && chown doppler:doppler /data \
    && chmod 755 /data

USER doppler

# Expose the web UI port (default; override with WEB_PORT env var)
EXPOSE 6096

HEALTHCHECK --interval=60s --timeout=5s --retries=3 \
    CMD ["/usr/local/bin/ubersdr_doppler", "-help"] || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
