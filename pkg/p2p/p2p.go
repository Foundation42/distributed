package p2p

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Foundation42/distributed/pkg/config"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	libp2pquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"
)

const (
	ProtocolID      = "/distributed/1.0.0"
	ServiceTopic    = "distributed-service"
	HealthTopic     = "distributed-health"
	DiscoveryTopic  = "distributed-discovery"
)

type Node struct {
	host       host.Host
	dht        *dht.IpfsDHT
	pubsub     *pubsub.PubSub
	ctx        context.Context
	cancel     context.CancelFunc
	config     *config.P2PConfig
	logger     *zap.Logger
	
	peers      map[peer.ID]*PeerInfo
	peersMutex sync.RWMutex
	
	healthSub  *pubsub.Subscription
	serviceSub *pubsub.Subscription
	
	handlers   map[protocol.ID]network.StreamHandler
}

type PeerInfo struct {
	ID              peer.ID                `json:"peer_id"`
	ModelID         string                 `json:"model_id"`
	ContextSize     int                    `json:"context_size"`
	TokensPerSecond int                    `json:"tokens_per_second"`
	GeoLocation     string                 `json:"geo_location"`
	WGPublicKey     string                 `json:"wg_public_key"`
	WGIPAddress     string                 `json:"wg_ip_address"`
	Endpoints       []string               `json:"endpoints"`
	Capabilities    map[string]interface{} `json:"capabilities"`
	LastSeen        time.Time              `json:"last_seen"`
	Health          *HealthMetrics         `json:"health"`
}

type HealthMetrics struct {
	UptimeSeconds  int64   `json:"uptime_seconds"`
	QueueDepth     int     `json:"queue_depth"`
	LoadAverage    float64 `json:"load_average"`
	LastHeartbeat  int64   `json:"last_heartbeat"`
	AvailableVRAM  int64   `json:"available_vram"`
	CPUUsage       float64 `json:"cpu_usage"`
}

func NewNode(cfg *config.P2PConfig, logger *zap.Logger) (*Node, error) {
	ctx, cancel := context.WithCancel(context.Background())
	
	// Generate or load private key
	var privKey crypto.PrivKey
	var err error
	
	if cfg.PrivateKey != "" {
		// Load existing key
		privKeyBytes := []byte(cfg.PrivateKey)
		privKey, err = crypto.UnmarshalPrivateKey(privKeyBytes)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to unmarshal private key: %w", err)
		}
	} else {
		// Generate new key
		privKey, _, err = crypto.GenerateKeyPairWithReader(crypto.Ed25519, 2048, rand.Reader)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to generate private key: %w", err)
		}
	}
	
	// Parse listen addresses
	listenAddrs := make([]multiaddr.Multiaddr, len(cfg.ListenAddrs))
	for i, addr := range cfg.ListenAddrs {
		maddr, err := multiaddr.NewMultiaddr(addr)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("invalid listen address %s: %w", addr, err)
		}
		listenAddrs[i] = maddr
	}
	
	// Connection manager
	connMgr, err := connmgr.NewConnManager(10, 100, connmgr.WithGracePeriod(time.Minute))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create connection manager: %w", err)
	}
	
	// Create libp2p host
	h, err := libp2p.New(
		libp2p.Identity(privKey),
		libp2p.ListenAddrs(listenAddrs...),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Transport(libp2pquic.NewTransport),
		libp2p.ConnectionManager(connMgr),
		libp2p.NATPortMap(),
		libp2p.EnableAutoRelay(),
		libp2p.EnableHolePunching(),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create libp2p host: %w", err)
	}
	
	// Create DHT
	var kadDHT *dht.IpfsDHT
	if cfg.EnableDHT {
		kadDHT, err = dht.New(ctx, h, dht.Mode(dht.ModeAutoServer))
		if err != nil {
			h.Close()
			cancel()
			return nil, fmt.Errorf("failed to create DHT: %w", err)
		}
	}
	
	// Create PubSub
	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		h.Close()
		cancel()
		return nil, fmt.Errorf("failed to create pubsub: %w", err)
	}
	
	node := &Node{
		host:     h,
		dht:      kadDHT,
		pubsub:   ps,
		ctx:      ctx,
		cancel:   cancel,
		config:   cfg,
		logger:   logger,
		peers:    make(map[peer.ID]*PeerInfo),
		handlers: make(map[protocol.ID]network.StreamHandler),
	}
	
	// Subscribe to topics
	if cfg.EnableGossip {
		if err := node.subscribeToTopics(); err != nil {
			node.Close()
			return nil, fmt.Errorf("failed to subscribe to topics: %w", err)
		}
	}
	
	// Start mDNS discovery
	if cfg.EnableMDNS {
		if err := node.setupMDNS(); err != nil {
			logger.Warn("Failed to setup mDNS discovery", zap.Error(err))
		}
	}
	
	// Bootstrap DHT
	if cfg.EnableDHT && len(cfg.BootstrapPeers) > 0 {
		if err := node.bootstrapDHT(); err != nil {
			logger.Warn("Failed to bootstrap DHT", zap.Error(err))
		}
	}
	
	// Start background tasks
	go node.heartbeatLoop()
	go node.peerDiscoveryLoop()
	
	logger.Info("P2P node started",
		zap.String("peer_id", h.ID().String()),
		zap.Strings("listen_addrs", cfg.ListenAddrs),
	)
	
	return node, nil
}

func (n *Node) subscribeToTopics() error {
	var err error
	
	// Subscribe to health topic
	healthTopic, err := n.pubsub.Join(HealthTopic)
	if err != nil {
		return fmt.Errorf("failed to join health topic: %w", err)
	}
	n.healthSub, err = healthTopic.Subscribe()
	if err != nil {
		return fmt.Errorf("failed to subscribe to health topic: %w", err)
	}
	
	// Subscribe to service topic
	serviceTopic, err := n.pubsub.Join(ServiceTopic)
	if err != nil {
		return fmt.Errorf("failed to join service topic: %w", err)
	}
	n.serviceSub, err = serviceTopic.Subscribe()
	if err != nil {
		return fmt.Errorf("failed to subscribe to service topic: %w", err)
	}
	
	// Start message handlers
	go n.handleHealthMessages()
	go n.handleServiceMessages()
	
	return nil
}

func (n *Node) setupMDNS() error {
	mdnsService := mdns.NewMdnsService(n.host, ServiceTopic, &mdnsNotifee{node: n})
	return mdnsService.Start()
}

type mdnsNotifee struct {
	node *Node
}

func (m *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	m.node.logger.Debug("Found peer via mDNS", zap.String("peer_id", pi.ID.String()))
	
	ctx, cancel := context.WithTimeout(m.node.ctx, 10*time.Second)
	defer cancel()
	
	if err := m.node.host.Connect(ctx, pi); err != nil {
		m.node.logger.Warn("Failed to connect to mDNS peer",
			zap.String("peer_id", pi.ID.String()),
			zap.Error(err),
		)
	}
}

func (n *Node) bootstrapDHT() error {
	for _, peerAddr := range n.config.BootstrapPeers {
		addr, err := multiaddr.NewMultiaddr(peerAddr)
		if err != nil {
			n.logger.Warn("Invalid bootstrap peer address", zap.String("addr", peerAddr), zap.Error(err))
			continue
		}
		
		peerInfo, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			n.logger.Warn("Failed to parse bootstrap peer", zap.String("addr", peerAddr), zap.Error(err))
			continue
		}
		
		ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
		if err := n.host.Connect(ctx, *peerInfo); err != nil {
			n.logger.Warn("Failed to connect to bootstrap peer",
				zap.String("peer_id", peerInfo.ID.String()),
				zap.Error(err),
			)
		}
		cancel()
	}
	
	return n.dht.Bootstrap(n.ctx)
}

func (n *Node) heartbeatLoop() {
	ticker := time.NewTicker(n.config.HeartbeatPeriod)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			n.broadcastHeartbeat()
		case <-n.ctx.Done():
			return
		}
	}
}

func (n *Node) broadcastHeartbeat() {
	// Implementation will be added
}

func (n *Node) peerDiscoveryLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			n.discoverPeers()
		case <-n.ctx.Done():
			return
		}
	}
}

func (n *Node) discoverPeers() {
	// Implementation will be added
}

func (n *Node) handleHealthMessages() {
	for {
		msg, err := n.healthSub.Next(n.ctx)
		if err != nil {
			return
		}
		
		var health HealthMetrics
		if err := json.Unmarshal(msg.Data, &health); err != nil {
			n.logger.Warn("Failed to unmarshal health message", zap.Error(err))
			continue
		}
		
		n.updatePeerHealth(msg.GetFrom(), &health)
	}
}

func (n *Node) handleServiceMessages() {
	for {
		msg, err := n.serviceSub.Next(n.ctx)
		if err != nil {
			return
		}
		
		var info PeerInfo
		if err := json.Unmarshal(msg.Data, &info); err != nil {
			n.logger.Warn("Failed to unmarshal service message", zap.Error(err))
			continue
		}
		
		n.updatePeerInfo(msg.GetFrom(), &info)
	}
}

func (n *Node) updatePeerHealth(peerID peer.ID, health *HealthMetrics) {
	n.peersMutex.Lock()
	defer n.peersMutex.Unlock()
	
	if peer, exists := n.peers[peerID]; exists {
		peer.Health = health
		peer.LastSeen = time.Now()
	}
}

func (n *Node) updatePeerInfo(peerID peer.ID, info *PeerInfo) {
	n.peersMutex.Lock()
	defer n.peersMutex.Unlock()
	
	info.ID = peerID
	info.LastSeen = time.Now()
	n.peers[peerID] = info
}

func (n *Node) GetPeers() []*PeerInfo {
	n.peersMutex.RLock()
	defer n.peersMutex.RUnlock()
	
	peers := make([]*PeerInfo, 0, len(n.peers))
	for _, peer := range n.peers {
		peers = append(peers, peer)
	}
	return peers
}

func (n *Node) Close() error {
	n.cancel()
	if n.dht != nil {
		n.dht.Close()
	}
	return n.host.Close()
}

func (n *Node) Host() host.Host {
	return n.host
}

func (n *Node) DHT() *dht.IpfsDHT {
	return n.dht
}

func (n *Node) PubSub() *pubsub.PubSub {
	return n.pubsub
}