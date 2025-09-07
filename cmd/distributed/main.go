package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Foundation42/distributed/pkg/config"
	"github.com/Foundation42/distributed/pkg/gateway"
	"github.com/Foundation42/distributed/pkg/llm"
	"github.com/Foundation42/distributed/pkg/p2p"
	"github.com/Foundation42/distributed/pkg/scheduler"
	"github.com/Foundation42/distributed/pkg/wg"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	cfgFile string
	logger  *zap.Logger
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "distributed",
		Short: "Distributed compute platform using libp2p and WireGuard",
		Long: `A decentralized platform for distributed computing workloads,
optimized for LLM inference but capable of handling any distributed task.`,
	}
	
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is ./distributed.yaml)")
	rootCmd.PersistentFlags().String("log-level", "info", "log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().String("data-dir", "/var/lib/distributed", "data directory")
	
	viper.BindPFlag("node.log_level", rootCmd.PersistentFlags().Lookup("log-level"))
	viper.BindPFlag("node.data_dir", rootCmd.PersistentFlags().Lookup("data-dir"))
	
	rootCmd.AddCommand(runCmd())
	rootCmd.AddCommand(versionCmd())
	
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the distributed node",
		RunE:  runNode,
	}
	
	// P2P flags
	cmd.Flags().StringSlice("listen-addrs", []string{"/ip4/0.0.0.0/tcp/4001"}, "P2P listen addresses")
	cmd.Flags().StringSlice("bootstrap-peers", []string{}, "Bootstrap peer addresses")
	cmd.Flags().Bool("enable-mdns", true, "Enable mDNS discovery")
	cmd.Flags().Bool("enable-dht", true, "Enable DHT")
	cmd.Flags().String("geo", "unknown", "Geographic location")
	
	// WireGuard flags
	cmd.Flags().Bool("wg-enabled", true, "Enable WireGuard overlay")
	cmd.Flags().String("wg-interface", "wg0", "WireGuard interface name")
	cmd.Flags().Int("wg-port", 51820, "WireGuard listen port")
	cmd.Flags().String("wg-subnet", "10.77.0.0/16", "WireGuard subnet CIDR")
	
	// LLM flags
	cmd.Flags().String("llm-backend", "llamacpp", "LLM backend (llamacpp)")
	cmd.Flags().String("llm-url", "http://127.0.0.1:8080/v1", "LLM server URL")
	cmd.Flags().String("model-id", "", "Model identifier")
	cmd.Flags().Int("context-size", 8192, "Model context size")
	cmd.Flags().Int("gpu-layers", -1, "Number of GPU layers (-1 for auto)")
	
	// Gateway flags
	cmd.Flags().Bool("gateway", false, "Enable HTTP gateway")
	cmd.Flags().String("gateway-addr", ":8088", "Gateway listen address")
	
	// Bind flags to viper
	viper.BindPFlag("p2p.listen_addrs", cmd.Flags().Lookup("listen-addrs"))
	viper.BindPFlag("p2p.bootstrap_peers", cmd.Flags().Lookup("bootstrap-peers"))
	viper.BindPFlag("p2p.enable_mdns", cmd.Flags().Lookup("enable-mdns"))
	viper.BindPFlag("p2p.enable_dht", cmd.Flags().Lookup("enable-dht"))
	viper.BindPFlag("node.geo_location", cmd.Flags().Lookup("geo"))
	
	viper.BindPFlag("wireguard.enabled", cmd.Flags().Lookup("wg-enabled"))
	viper.BindPFlag("wireguard.interface", cmd.Flags().Lookup("wg-interface"))
	viper.BindPFlag("wireguard.listen_port", cmd.Flags().Lookup("wg-port"))
	viper.BindPFlag("wireguard.subnet_cidr", cmd.Flags().Lookup("wg-subnet"))
	
	viper.BindPFlag("llm.backend", cmd.Flags().Lookup("llm-backend"))
	viper.BindPFlag("llm.llamacpp_url", cmd.Flags().Lookup("llm-url"))
	viper.BindPFlag("llm.model_id", cmd.Flags().Lookup("model-id"))
	viper.BindPFlag("llm.context_size", cmd.Flags().Lookup("context-size"))
	viper.BindPFlag("llm.gpu_layers", cmd.Flags().Lookup("gpu-layers"))
	
	viper.BindPFlag("gateway.enabled", cmd.Flags().Lookup("gateway"))
	viper.BindPFlag("gateway.listen_addr", cmd.Flags().Lookup("gateway-addr"))
	
	return cmd
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("Distributed Platform v0.1.0")
		},
	}
}

func runNode(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	
	// Initialize logger
	if err := initLogger(cfg.Node.LogLevel); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	defer logger.Sync()
	
	logger.Info("Starting distributed node",
		zap.String("version", "0.1.0"),
		zap.String("geo", cfg.Node.GeoLocation),
	)
	
	// Create data directory
	if err := os.MkdirAll(cfg.Node.DataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}
	
	// Initialize P2P node
	p2pNode, err := p2p.NewNode(&cfg.P2P, logger.Named("p2p"))
	if err != nil {
		return fmt.Errorf("failed to create P2P node: %w", err)
	}
	defer p2pNode.Close()
	
	logger.Info("P2P node started",
		zap.String("peer_id", p2pNode.Host().ID().String()),
		zap.Strings("listen_addrs", cfg.P2P.ListenAddrs),
	)
	
	// Initialize WireGuard manager if enabled
	var wgManager *wg.Manager
	if cfg.WireGuard.Enabled {
		wgManager, err = wg.NewManager(&cfg.WireGuard, logger.Named("wireguard"))
		if err != nil {
			logger.Warn("Failed to initialize WireGuard", zap.Error(err))
			// Continue without WireGuard
		} else {
			defer wgManager.Close()
		}
	}
	
	// Initialize LLM adapter
	var llmAdapter llm.Adapter
	if cfg.LLM.Backend == "llamacpp" && cfg.LLM.LlamaCPPURL != "" {
		llmAdapter, err = llm.NewLlamaCPPAdapter(&cfg.LLM, logger.Named("llm"))
		if err != nil {
			logger.Warn("Failed to initialize LLM adapter", zap.Error(err))
			// Continue without LLM
		} else {
			defer llmAdapter.Close()
		}
	}
	
	// Initialize scheduler
	sched := scheduler.NewScheduler(&cfg.Scheduler, p2pNode, logger.Named("scheduler"))
	
	// Initialize and start gateway if enabled
	if cfg.Gateway.Enabled {
		gw := gateway.NewGateway(&cfg.Gateway, p2pNode, sched, llmAdapter, logger.Named("gateway"))
		go func() {
			if err := gw.Start(); err != nil {
				logger.Error("Gateway failed", zap.Error(err))
			}
		}()
		defer gw.Stop()
		
		logger.Info("HTTP gateway started", zap.String("addr", cfg.Gateway.ListenAddr))
	}
	
	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	
	// Main loop
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-sigChan:
			logger.Info("Shutting down...")
			return nil
			
		case <-ticker.C:
			// Log status
			peers := p2pNode.GetPeers()
			logger.Info("Node status",
				zap.Int("connected_peers", len(peers)),
				zap.Bool("wireguard", wgManager != nil),
				zap.Bool("llm", llmAdapter != nil),
			)
		}
	}
}

func loadConfig() (*config.Config, error) {
	cfg := config.DefaultConfig()
	
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		viper.SetConfigName("distributed")
		viper.SetConfigType("yaml")
		viper.AddConfigPath(".")
		viper.AddConfigPath("/etc/distributed/")
	}
	
	viper.SetEnvPrefix("DISTRIBUTED")
	viper.AutomaticEnv()
	
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
		// Config file not found; use defaults
	}
	
	if err := viper.Unmarshal(cfg); err != nil {
		return nil, err
	}
	
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	
	return cfg, nil
}

func initLogger(level string) error {
	var zapLevel zapcore.Level
	if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
		return err
	}
	
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapLevel)
	config.OutputPaths = []string{"stdout"}
	config.ErrorOutputPaths = []string{"stderr"}
	
	var err error
	logger, err = config.Build()
	return err
}