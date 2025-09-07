# Multi-stage build for minimal image size
FROM golang:1.21-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev linux-headers

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o distributed ./cmd/distributed

# Final stage - Alpine runtime image
FROM alpine:3.19

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    iptables \
    ip6tables \
    iproute2 \
    wireguard-tools \
    bash \
    curl \
    && rm -rf /var/cache/apk/*

# Install wireguard-go for userspace fallback
RUN apk add --no-cache --repository=http://dl-cdn.alpinelinux.org/alpine/edge/testing wireguard-go

# Create non-root user
RUN addgroup -g 1000 distributed && \
    adduser -D -u 1000 -G distributed distributed

# Create data directory
RUN mkdir -p /var/lib/distributed && \
    chown -R distributed:distributed /var/lib/distributed

# Copy binary from builder
COPY --from=builder /build/distributed /usr/local/bin/distributed

# Set up entrypoint script
COPY --from=builder /build/deploy/docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# Expose ports
EXPOSE 4001 51820/udp 8088 9090

# Health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD curl -f http://localhost:9090/metrics || exit 1

ENTRYPOINT ["/entrypoint.sh"]
CMD ["run"]