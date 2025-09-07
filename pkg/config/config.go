package config

import (
	"fmt"
	"time"
)

type Config struct {
	Node       NodeConfig       `mapstructure:"node"`
	P2P        P2PConfig        `mapstructure:"p2p"`
	WireGuard  WireGuardConfig  `mapstructure:"wireguard"`
	LLM        LLMConfig        `mapstructure:"llm"`
	Scheduler  SchedulerConfig  `mapstructure:"scheduler"`
	Gateway    GatewayConfig    `mapstructure:"gateway"`
	Monitoring MonitoringConfig `mapstructure:"monitoring"`
}

type NodeConfig struct {
	ID          string `mapstructure:"id"`
	DataDir     string `mapstructure:"data_dir"`
	GeoLocation string `mapstructure:"geo_location"`
	LogLevel    string `mapstructure:"log_level"`
}

type P2PConfig struct {
	ListenAddrs     []string      `mapstructure:"listen_addrs"`
	BootstrapPeers  []string      `mapstructure:"bootstrap_peers"`
	EnableMDNS      bool          `mapstructure:"enable_mdns"`
	EnableDHT       bool          `mapstructure:"enable_dht"`
	EnableGossip    bool          `mapstructure:"enable_gossip"`
	HeartbeatPeriod time.Duration `mapstructure:"heartbeat_period"`
	PrivateKey      string        `mapstructure:"private_key"`
}

type WireGuardConfig struct {
	Enabled         bool     `mapstructure:"enabled"`
	Interface       string   `mapstructure:"interface"`
	PrivateKey      string   `mapstructure:"private_key"`
	ListenPort      int      `mapstructure:"listen_port"`
	SubnetCIDR      string   `mapstructure:"subnet_cidr"`
	AllowedIPs      []string `mapstructure:"allowed_ips"`
	PersistentKeep  int      `mapstructure:"persistent_keepalive"`
	UseKernelModule bool     `mapstructure:"use_kernel_module"`
}

type LLMConfig struct {
	Backend         string            `mapstructure:"backend"`
	ModelID         string            `mapstructure:"model_id"`
	ModelPath       string            `mapstructure:"model_path"`
	ContextSize     int               `mapstructure:"context_size"`
	BatchSize       int               `mapstructure:"batch_size"`
	Threads         int               `mapstructure:"threads"`
	GPULayers       int               `mapstructure:"gpu_layers"`
	LlamaCPPURL     string            `mapstructure:"llamacpp_url"`
	MaxConcurrent   int               `mapstructure:"max_concurrent"`
	TokensPerSecond int               `mapstructure:"tokens_per_second"`
	Capabilities    map[string]string `mapstructure:"capabilities"`
}

type SchedulerConfig struct {
	Strategy        string        `mapstructure:"strategy"`
	MaxRetries      int           `mapstructure:"max_retries"`
	RequestTimeout  time.Duration `mapstructure:"request_timeout"`
	HealthCheckRate time.Duration `mapstructure:"health_check_rate"`
	LoadBalancing   string        `mapstructure:"load_balancing"`
}

type GatewayConfig struct {
	Enabled    bool   `mapstructure:"enabled"`
	ListenAddr string `mapstructure:"listen_addr"`
	EnableTLS  bool   `mapstructure:"enable_tls"`
	CertFile   string `mapstructure:"cert_file"`
	KeyFile    string `mapstructure:"key_file"`
}

type MonitoringConfig struct {
	MetricsEnabled bool   `mapstructure:"metrics_enabled"`
	MetricsAddr    string `mapstructure:"metrics_addr"`
	TracingEnabled bool   `mapstructure:"tracing_enabled"`
	TracingAddr    string `mapstructure:"tracing_addr"`
}

func DefaultConfig() *Config {
	return &Config{
		Node: NodeConfig{
			DataDir:     "/var/lib/distributed",
			GeoLocation: "unknown",
			LogLevel:    "info",
		},
		P2P: P2PConfig{
			ListenAddrs:     []string{"/ip4/0.0.0.0/tcp/4001"},
			EnableMDNS:      true,
			EnableDHT:       true,
			EnableGossip:    true,
			HeartbeatPeriod: 30 * time.Second,
		},
		WireGuard: WireGuardConfig{
			Enabled:         true,
			Interface:       "wg0",
			ListenPort:      51820,
			SubnetCIDR:      "10.77.0.0/16",
			PersistentKeep:  25,
			UseKernelModule: true,
		},
		LLM: LLMConfig{
			Backend:         "llamacpp",
			ContextSize:     8192,
			BatchSize:       512,
			Threads:         8,
			GPULayers:       -1,
			LlamaCPPURL:     "http://127.0.0.1:8080/v1",
			MaxConcurrent:   4,
			TokensPerSecond: 100,
		},
		Scheduler: SchedulerConfig{
			Strategy:        "latency_aware",
			MaxRetries:      3,
			RequestTimeout:  5 * time.Minute,
			HealthCheckRate: 10 * time.Second,
			LoadBalancing:   "round_robin",
		},
		Gateway: GatewayConfig{
			Enabled:    false,
			ListenAddr: ":8088",
			EnableTLS:  false,
		},
		Monitoring: MonitoringConfig{
			MetricsEnabled: true,
			MetricsAddr:    ":9090",
			TracingEnabled: false,
		},
	}
}

func (c *Config) Validate() error {
	if c.Node.DataDir == "" {
		return fmt.Errorf("node.data_dir is required")
	}
	if c.WireGuard.Enabled && c.WireGuard.SubnetCIDR == "" {
		return fmt.Errorf("wireguard.subnet_cidr is required when WireGuard is enabled")
	}
	if c.LLM.Backend == "" {
		return fmt.Errorf("llm.backend is required")
	}
	return nil
}