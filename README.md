# Distributed Platform

A decentralized compute platform using libp2p for control plane and WireGuard for data plane, optimized for distributed LLM inference but capable of handling any distributed workload.

## Features

- **P2P Control Plane**: Built on libp2p with DHT, mDNS discovery, and GossipSub for health monitoring
- **WireGuard Data Plane**: Secure, high-performance encrypted overlay network
- **LLM Support**: Native integration with llama.cpp for distributed inference
- **Smart Scheduling**: Multiple strategies including latency-aware, capability-based, and geo-proximity routing
- **HTTP Gateway**: OpenAI-compatible API for non-p2p clients
- **Alpine-based**: Minimal Docker images using Alpine Linux for security and efficiency
- **Metrics & Monitoring**: Prometheus metrics and health endpoints

## Quick Start

### Prerequisites

- Docker and Docker Compose
- A GGUF model file (e.g., `llama-3.1-8b-q5_K_M.gguf`)

### 1. Clone the repository

```bash
git clone https://github.com/Foundation42/distributed.git
cd distributed
```

### 2. Place your model file

```bash
mkdir -p models
# Copy your GGUF model to models/llama-3.1-8b-q5_K_M.gguf
```

### 3. Start the distributed network

```bash
docker-compose -f deploy/compose/docker-compose.yml up --build
```

This will start:
- 3 distributed nodes (us-east, us-west, eu-central)
- 3 llama.cpp servers
- WireGuard overlay network
- HTTP gateway on port 8088

### 4. Test the system

```bash
# Generate text
curl -X POST http://localhost:8088/generate \
  -H "Content-Type: application/json" \
  -d '{
    "prompt": "What is the meaning of life?",
    "max_tokens": 100
  }'

# Check health
curl http://localhost:8088/health

# View connected peers
curl http://localhost:8088/peers

# View metrics
curl http://localhost:9090/metrics
```

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                   HTTP Gateway                       │
│              (OpenAI-compatible API)                 │
└─────────────────────────────────────────────────────┘
                          │
┌─────────────────────────────────────────────────────┐
│                  Control Plane                       │
│                    (libp2p)                         │
│  • DHT for service discovery                        │
│  • GossipSub for health monitoring                  │
│  • mDNS for local discovery                         │
└─────────────────────────────────────────────────────┘
                          │
┌─────────────────────────────────────────────────────┐
│                   Data Plane                         │
│                  (WireGuard)                        │
│  • Encrypted overlay network                        │
│  • Deterministic IP addressing                      │
│  • NAT traversal & hole punching                    │
└─────────────────────────────────────────────────────┘
                          │
┌─────────────────────────────────────────────────────┐
│                 Compute Backend                      │
│                  (llama.cpp)                        │
│  • Local inference                                  │
│  • Model sharding support                           │
│  • GPU acceleration                                 │
└─────────────────────────────────────────────────────┘
```

## Configuration

### Environment Variables

```bash
# Node configuration
DISTRIBUTED_NODE_GEO_LOCATION=us-east
DISTRIBUTED_NODE_LOG_LEVEL=info

# P2P configuration
DISTRIBUTED_P2P_LISTEN_ADDRS=/ip4/0.0.0.0/tcp/4001
DISTRIBUTED_P2P_ENABLE_MDNS=true
DISTRIBUTED_P2P_ENABLE_DHT=true
DISTRIBUTED_P2P_BOOTSTRAP_PEERS=

# WireGuard configuration
DISTRIBUTED_WIREGUARD_ENABLED=true
DISTRIBUTED_WIREGUARD_LISTEN_PORT=51820
DISTRIBUTED_WIREGUARD_SUBNET_CIDR=10.77.0.0/16

# LLM configuration
DISTRIBUTED_LLM_BACKEND=llamacpp
DISTRIBUTED_LLM_LLAMACPP_URL=http://127.0.0.1:8080/v1
DISTRIBUTED_LLM_MODEL_ID=llama-3.1-8b
DISTRIBUTED_LLM_CONTEXT_SIZE=8192

# Gateway configuration
DISTRIBUTED_GATEWAY_ENABLED=true
DISTRIBUTED_GATEWAY_LISTEN_ADDR=:8088
```

### Configuration File

Create a `distributed.yaml`:

```yaml
node:
  geo_location: us-east
  log_level: info
  data_dir: /var/lib/distributed

p2p:
  listen_addrs:
    - /ip4/0.0.0.0/tcp/4001
  enable_mdns: true
  enable_dht: true
  enable_gossip: true
  heartbeat_period: 30s

wireguard:
  enabled: true
  interface: wg0
  listen_port: 51820
  subnet_cidr: 10.77.0.0/16
  persistent_keepalive: 25

llm:
  backend: llamacpp
  llamacpp_url: http://127.0.0.1:8080/v1
  model_id: llama-3.1-8b
  context_size: 8192
  tokens_per_second: 100

scheduler:
  strategy: latency_aware
  health_check_rate: 10s
  request_timeout: 5m

gateway:
  enabled: true
  listen_addr: :8088
```

## Building from Source

### Requirements

- Go 1.21+
- Alpine Linux or compatible (for WireGuard kernel module)
- Protocol Buffers compiler

### Build

```bash
# Install dependencies
go mod download

# Generate protobuf code
protoc --go_out=. --go-grpc_out=. api/proto/distributed.proto

# Build binary
go build -o distributed ./cmd/distributed

# Run
./distributed run --config distributed.yaml
```

## Deployment

### Single Node

```bash
docker run -d \
  --name distributed-node \
  --cap-add NET_ADMIN \
  --cap-add SYS_MODULE \
  -p 4001:4001 \
  -p 51820:51820/udp \
  -p 8088:8088 \
  -v $(pwd)/models:/models \
  -e DISTRIBUTED_GATEWAY_ENABLED=true \
  ghcr.io/foundation42/distributed:latest
```

### Kubernetes

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: distributed
spec:
  selector:
    matchLabels:
      app: distributed
  template:
    metadata:
      labels:
        app: distributed
    spec:
      hostNetwork: true
      containers:
      - name: distributed
        image: ghcr.io/foundation42/distributed:latest
        securityContext:
          capabilities:
            add:
              - NET_ADMIN
              - SYS_MODULE
        env:
        - name: DISTRIBUTED_NODE_GEO_LOCATION
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        volumeMounts:
        - name: models
          mountPath: /models
        - name: lib-modules
          mountPath: /lib/modules
          readOnly: true
      volumes:
      - name: models
        hostPath:
          path: /var/lib/distributed/models
      - name: lib-modules
        hostPath:
          path: /lib/modules
```

## API Reference

### Generate Text

```bash
POST /generate
{
  "prompt": "Hello, world!",
  "max_tokens": 100,
  "temperature": 0.7,
  "stream": false
}
```

### OpenAI-Compatible Endpoints

- `POST /v1/completions` - Text completion
- `POST /v1/chat/completions` - Chat completion
- `POST /v1/embeddings` - Generate embeddings
- `GET /v1/models` - List available models

### Management Endpoints

- `GET /health` - Health status
- `GET /metrics` - Prometheus metrics
- `GET /peers` - List connected peers
- `GET /peers/metrics` - Peer performance metrics

## Scheduling Strategies

- **round_robin**: Distribute requests evenly
- **least_loaded**: Route to least busy peer
- **latency_aware**: Consider network latency (default)
- **capability_based**: Match hardware requirements
- **geo_proximity**: Prefer geographically close peers

## Security

- **P2P**: TLS + Noise protocol for peer connections
- **WireGuard**: ChaCha20Poly1305 encryption
- **API**: Optional TLS for HTTP gateway
- **Isolation**: Alpine Linux containers with minimal attack surface

## Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## License

MIT License - see [LICENSE](LICENSE) for details.

## Acknowledgments

- [libp2p](https://libp2p.io/) for P2P networking
- [WireGuard](https://www.wireguard.com/) for secure networking
- [llama.cpp](https://github.com/ggerganov/llama.cpp) for LLM inference
- Alpine Linux for minimal containers