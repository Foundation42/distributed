package wg

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Foundation42/distributed/pkg/config"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type Manager struct {
	config     *config.WireGuardConfig
	logger     *zap.Logger
	client     *wgctrl.Client
	privateKey wgtypes.Key
	publicKey  wgtypes.Key
	ipAddress  net.IP
	ipNet      *net.IPNet
	peers      map[string]*WGPeer
	peersMutex sync.RWMutex
	isUserspace bool
}

type WGPeer struct {
	PublicKey       string
	Endpoint        *net.UDPAddr
	AllowedIPs      []net.IPNet
	LastHandshake   time.Time
	PersistentKeep  time.Duration
	BytesReceived   int64
	BytesTransmitted int64
}

func NewManager(cfg *config.WireGuardConfig, logger *zap.Logger) (*Manager, error) {
	m := &Manager{
		config: cfg,
		logger: logger,
		peers:  make(map[string]*WGPeer),
	}
	
	// Parse subnet CIDR
	_, ipNet, err := net.ParseCIDR(cfg.SubnetCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet CIDR: %w", err)
	}
	m.ipNet = ipNet
	
	// Generate or load WireGuard keys
	if err := m.setupKeys(); err != nil {
		return nil, fmt.Errorf("failed to setup keys: %w", err)
	}
	
	// Initialize WireGuard interface
	if err := m.initInterface(); err != nil {
		return nil, fmt.Errorf("failed to initialize interface: %w", err)
	}
	
	logger.Info("WireGuard manager initialized",
		zap.String("interface", cfg.Interface),
		zap.String("public_key", base64.StdEncoding.EncodeToString(m.publicKey[:])),
		zap.String("ip_address", m.ipAddress.String()),
		zap.Bool("userspace", m.isUserspace),
	)
	
	return m, nil
}

func (m *Manager) setupKeys() error {
	keyFile := filepath.Join(m.config.Interface + "_privatekey")
	
	if m.config.PrivateKey != "" {
		// Use provided private key
		keyBytes, err := base64.StdEncoding.DecodeString(m.config.PrivateKey)
		if err != nil {
			return fmt.Errorf("failed to decode private key: %w", err)
		}
		copy(m.privateKey[:], keyBytes)
	} else if data, err := os.ReadFile(keyFile); err == nil {
		// Load existing key from file
		keyStr := string(data)
		key, err := wgtypes.ParseKey(keyStr)
		if err != nil {
			return fmt.Errorf("failed to parse private key: %w", err)
		}
		m.privateKey = key
	} else {
		// Generate new key
		key, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return fmt.Errorf("failed to generate private key: %w", err)
		}
		m.privateKey = key
		
		// Save key to file
		if err := os.WriteFile(keyFile, []byte(key.String()), 0600); err != nil {
			m.logger.Warn("Failed to save private key to file", zap.Error(err))
		}
	}
	
	// Derive public key
	m.publicKey = m.privateKey.PublicKey()
	
	return nil
}

func (m *Manager) initInterface() error {
	// Check if kernel module is available
	if m.config.UseKernelModule && m.isKernelModuleAvailable() {
		if err := m.initKernelInterface(); err == nil {
			m.isUserspace = false
			return nil
		}
		m.logger.Warn("Failed to use kernel WireGuard, falling back to userspace")
	}
	
	// Fall back to userspace implementation
	return m.initUserspaceInterface()
}

func (m *Manager) isKernelModuleAvailable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	
	// Check if wireguard module is loaded
	if _, err := os.Stat("/sys/module/wireguard"); err == nil {
		return true
	}
	
	// Try to load the module
	if err := exec.Command("modprobe", "wireguard").Run(); err == nil {
		return true
	}
	
	return false
}

func (m *Manager) initKernelInterface() error {
	// Create WireGuard client
	client, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("failed to create wgctrl client: %w", err)
	}
	m.client = client
	
	// Check if interface exists
	if _, err := client.Device(m.config.Interface); err != nil {
		// Create interface
		if err := m.createKernelInterface(); err != nil {
			return fmt.Errorf("failed to create interface: %w", err)
		}
	}
	
	// Configure interface
	cfg := wgtypes.Config{
		PrivateKey:   &m.privateKey,
		ListenPort:   &m.config.ListenPort,
		ReplacePeers: false,
	}
	
	if err := client.ConfigureDevice(m.config.Interface, cfg); err != nil {
		return fmt.Errorf("failed to configure device: %w", err)
	}
	
	// Assign IP address
	if err := m.assignIPAddress(); err != nil {
		return fmt.Errorf("failed to assign IP address: %w", err)
	}
	
	// Bring interface up
	if err := m.bringInterfaceUp(); err != nil {
		return fmt.Errorf("failed to bring interface up: %w", err)
	}
	
	return nil
}

func (m *Manager) createKernelInterface() error {
	cmd := exec.Command("ip", "link", "add", m.config.Interface, "type", "wireguard")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create interface: %s", output)
	}
	return nil
}

func (m *Manager) initUserspaceInterface() error {
	m.isUserspace = true
	
	// Check if wireguard-go is available
	if _, err := exec.LookPath("wireguard-go"); err != nil {
		return fmt.Errorf("wireguard-go not found in PATH: %w", err)
	}
	
	// Start wireguard-go
	cmd := exec.Command("wireguard-go", m.config.Interface)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start wireguard-go: %w", err)
	}
	
	// Wait for interface to be created
	time.Sleep(2 * time.Second)
	
	// Create WireGuard client
	client, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("failed to create wgctrl client: %w", err)
	}
	m.client = client
	
	// Configure interface
	cfg := wgtypes.Config{
		PrivateKey:   &m.privateKey,
		ListenPort:   &m.config.ListenPort,
		ReplacePeers: false,
	}
	
	if err := client.ConfigureDevice(m.config.Interface, cfg); err != nil {
		return fmt.Errorf("failed to configure device: %w", err)
	}
	
	// Assign IP address
	if err := m.assignIPAddress(); err != nil {
		return fmt.Errorf("failed to assign IP address: %w", err)
	}
	
	// Bring interface up
	if err := m.bringInterfaceUp(); err != nil {
		return fmt.Errorf("failed to bring interface up: %w", err)
	}
	
	return nil
}

func (m *Manager) assignIPAddress() error {
	// Generate deterministic IP from public key
	m.ipAddress = m.generateIPFromKey(m.publicKey)
	
	// Assign IP to interface
	addr := fmt.Sprintf("%s/32", m.ipAddress.String())
	cmd := exec.Command("ip", "addr", "add", addr, "dev", m.config.Interface)
	if output, err := cmd.CombinedOutput(); err != nil {
		// Check if address already exists
		if !contains(string(output), "File exists") {
			return fmt.Errorf("failed to assign IP address: %s", output)
		}
	}
	
	return nil
}

func (m *Manager) bringInterfaceUp() error {
	cmd := exec.Command("ip", "link", "set", m.config.Interface, "up")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to bring interface up: %s", output)
	}
	return nil
}

func (m *Manager) generateIPFromKey(key wgtypes.Key) net.IP {
	// Hash the public key to generate a deterministic IP
	hash := sha256.Sum256(key[:])
	
	// Use the first 2 bytes of the hash for the last two octets
	// Assuming subnet is 10.77.0.0/16
	ip := make(net.IP, 4)
	ip[0] = m.ipNet.IP[0]
	ip[1] = m.ipNet.IP[1]
	ip[2] = hash[0]
	ip[3] = hash[1]
	
	// Ensure IP is within subnet and not network or broadcast address
	if ip[3] == 0 {
		ip[3] = 1
	} else if ip[3] == 255 {
		ip[3] = 254
	}
	
	return ip
}

func (m *Manager) AddPeer(publicKey string, endpoint string, allowedIPs []string) error {
	m.peersMutex.Lock()
	defer m.peersMutex.Unlock()
	
	// Parse public key
	key, err := wgtypes.ParseKey(publicKey)
	if err != nil {
		return fmt.Errorf("invalid public key: %w", err)
	}
	
	// Parse endpoint
	var udpAddr *net.UDPAddr
	if endpoint != "" {
		udpAddr, err = net.ResolveUDPAddr("udp", endpoint)
		if err != nil {
			return fmt.Errorf("invalid endpoint: %w", err)
		}
	}
	
	// Parse allowed IPs
	allowedNets := make([]net.IPNet, len(allowedIPs))
	for i, ipStr := range allowedIPs {
		_, ipNet, err := net.ParseCIDR(ipStr)
		if err != nil {
			return fmt.Errorf("invalid allowed IP %s: %w", ipStr, err)
		}
		allowedNets[i] = *ipNet
	}
	
	// Configure peer
	keepAlive := time.Duration(m.config.PersistentKeep) * time.Second
	peerCfg := wgtypes.PeerConfig{
		PublicKey:                   key,
		Endpoint:                    udpAddr,
		PersistentKeepaliveInterval: &keepAlive,
		ReplaceAllowedIPs:          true,
		AllowedIPs:                 allowedNets,
	}
	
	cfg := wgtypes.Config{
		Peers: []wgtypes.PeerConfig{peerCfg},
	}
	
	if err := m.client.ConfigureDevice(m.config.Interface, cfg); err != nil {
		return fmt.Errorf("failed to configure peer: %w", err)
	}
	
	// Store peer info
	m.peers[publicKey] = &WGPeer{
		PublicKey:      publicKey,
		Endpoint:       udpAddr,
		AllowedIPs:     allowedNets,
		PersistentKeep: keepAlive,
	}
	
	m.logger.Info("Added WireGuard peer",
		zap.String("public_key", publicKey),
		zap.String("endpoint", endpoint),
	)
	
	return nil
}

func (m *Manager) RemovePeer(publicKey string) error {
	m.peersMutex.Lock()
	defer m.peersMutex.Unlock()
	
	key, err := wgtypes.ParseKey(publicKey)
	if err != nil {
		return fmt.Errorf("invalid public key: %w", err)
	}
	
	peerCfg := wgtypes.PeerConfig{
		PublicKey: key,
		Remove:    true,
	}
	
	cfg := wgtypes.Config{
		Peers: []wgtypes.PeerConfig{peerCfg},
	}
	
	if err := m.client.ConfigureDevice(m.config.Interface, cfg); err != nil {
		return fmt.Errorf("failed to remove peer: %w", err)
	}
	
	delete(m.peers, publicKey)
	
	m.logger.Info("Removed WireGuard peer", zap.String("public_key", publicKey))
	
	return nil
}

func (m *Manager) GetPublicKey() string {
	return base64.StdEncoding.EncodeToString(m.publicKey[:])
}

func (m *Manager) GetIPAddress() string {
	return m.ipAddress.String()
}

func (m *Manager) GetPeers() map[string]*WGPeer {
	m.peersMutex.RLock()
	defer m.peersMutex.RUnlock()
	
	peers := make(map[string]*WGPeer)
	for k, v := range m.peers {
		peers[k] = v
	}
	return peers
}

func (m *Manager) GenerateIPForPeer(peerID peer.ID) string {
	// Generate deterministic IP from peer ID
	hash := sha256.Sum256([]byte(peerID.String()))
	
	ip := make(net.IP, 4)
	ip[0] = m.ipNet.IP[0]
	ip[1] = m.ipNet.IP[1]
	ip[2] = hash[0]
	ip[3] = hash[1]
	
	if ip[3] == 0 {
		ip[3] = 1
	} else if ip[3] == 255 {
		ip[3] = 254
	}
	
	return fmt.Sprintf("%s/32", ip.String())
}

func (m *Manager) Close() error {
	if m.client != nil {
		m.client.Close()
	}
	
	// Remove interface if userspace
	if m.isUserspace {
		exec.Command("ip", "link", "del", m.config.Interface).Run()
	}
	
	return nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[len(s)-len(substr):] == substr
}