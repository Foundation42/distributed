package scheduler

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/Foundation42/distributed/pkg/config"
	"github.com/Foundation42/distributed/pkg/p2p"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"
)

type Strategy string

const (
	StrategyRoundRobin    Strategy = "round_robin"
	StrategyLeastLoaded   Strategy = "least_loaded"
	StrategyLatencyAware  Strategy = "latency_aware"
	StrategyCapabilityBased Strategy = "capability_based"
	StrategyGeoProximity Strategy = "geo_proximity"
)

type Scheduler struct {
	config       *config.SchedulerConfig
	logger       *zap.Logger
	p2pNode      *p2p.Node
	
	peers        map[peer.ID]*PeerMetrics
	peersMutex   sync.RWMutex
	
	roundRobinIdx int
	rrMutex      sync.Mutex
	
	latencyMap   map[peer.ID]*LatencyMetrics
	latencyMutex sync.RWMutex
}

type PeerMetrics struct {
	PeerID          peer.ID
	ModelID         string
	ContextSize     int
	TokensPerSecond int
	GeoLocation     string
	
	// Runtime metrics
	QueueDepth      int
	LoadAverage     float64
	AvailableVRAM   int64
	CPUUsage        float64
	LastHeartbeat   time.Time
	
	// Performance metrics
	RequestCount    int64
	ErrorCount      int64
	TotalLatency    time.Duration
	AverageLatency  time.Duration
	
	// Capabilities
	Capabilities    map[string]interface{}
	
	// Scoring
	Score           float64
}

type LatencyMetrics struct {
	RTT             time.Duration
	PacketLoss      float64
	Jitter          time.Duration
	LastMeasured    time.Time
	MeasurementCount int
}

type SelectionCriteria struct {
	ModelID         string
	RequiredContext int
	MinVRAM         int64
	MaxLatency      time.Duration
	PreferredGeo    string
	Capabilities    map[string]interface{}
}

func NewScheduler(cfg *config.SchedulerConfig, p2pNode *p2p.Node, logger *zap.Logger) *Scheduler {
	s := &Scheduler{
		config:     cfg,
		logger:     logger,
		p2pNode:    p2pNode,
		peers:      make(map[peer.ID]*PeerMetrics),
		latencyMap: make(map[peer.ID]*LatencyMetrics),
	}
	
	// Start background tasks
	go s.metricsUpdateLoop()
	go s.latencyProbeLoop()
	go s.healthCheckLoop()
	
	logger.Info("Scheduler initialized",
		zap.String("strategy", cfg.Strategy),
		zap.Duration("health_check_rate", cfg.HealthCheckRate),
	)
	
	return s
}

func (s *Scheduler) SelectPeer(ctx context.Context, criteria *SelectionCriteria) (*PeerMetrics, error) {
	s.peersMutex.RLock()
	defer s.peersMutex.RUnlock()
	
	// Filter eligible peers
	eligible := s.filterEligiblePeers(criteria)
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no eligible peers found for model %s", criteria.ModelID)
	}
	
	// Apply selection strategy
	var selected *PeerMetrics
	
	switch Strategy(s.config.Strategy) {
	case StrategyRoundRobin:
		selected = s.selectRoundRobin(eligible)
	case StrategyLeastLoaded:
		selected = s.selectLeastLoaded(eligible)
	case StrategyLatencyAware:
		selected = s.selectLatencyAware(eligible)
	case StrategyCapabilityBased:
		selected = s.selectCapabilityBased(eligible, criteria)
	case StrategyGeoProximity:
		selected = s.selectGeoProximity(eligible, criteria)
	default:
		selected = s.selectLatencyAware(eligible)
	}
	
	if selected == nil {
		return nil, fmt.Errorf("failed to select peer")
	}
	
	s.logger.Debug("Selected peer",
		zap.String("peer_id", selected.PeerID.String()),
		zap.String("model_id", selected.ModelID),
		zap.Float64("score", selected.Score),
	)
	
	return selected, nil
}

func (s *Scheduler) filterEligiblePeers(criteria *SelectionCriteria) []*PeerMetrics {
	var eligible []*PeerMetrics
	
	for _, peer := range s.peers {
		// Check if peer is healthy
		if time.Since(peer.LastHeartbeat) > 2*s.config.HealthCheckRate {
			continue
		}
		
		// Check model match
		if criteria.ModelID != "" && peer.ModelID != criteria.ModelID {
			continue
		}
		
		// Check context size
		if criteria.RequiredContext > 0 && peer.ContextSize < criteria.RequiredContext {
			continue
		}
		
		// Check VRAM
		if criteria.MinVRAM > 0 && peer.AvailableVRAM < criteria.MinVRAM {
			continue
		}
		
		// Check capabilities
		if !s.matchesCapabilities(peer.Capabilities, criteria.Capabilities) {
			continue
		}
		
		eligible = append(eligible, peer)
	}
	
	return eligible
}

func (s *Scheduler) matchesCapabilities(peerCaps, requiredCaps map[string]interface{}) bool {
	for key, reqValue := range requiredCaps {
		peerValue, exists := peerCaps[key]
		if !exists {
			return false
		}
		
		// Simple equality check for now
		if peerValue != reqValue {
			return false
		}
	}
	return true
}

func (s *Scheduler) selectRoundRobin(peers []*PeerMetrics) *PeerMetrics {
	s.rrMutex.Lock()
	defer s.rrMutex.Unlock()
	
	if len(peers) == 0 {
		return nil
	}
	
	selected := peers[s.roundRobinIdx%len(peers)]
	s.roundRobinIdx++
	
	return selected
}

func (s *Scheduler) selectLeastLoaded(peers []*PeerMetrics) *PeerMetrics {
	if len(peers) == 0 {
		return nil
	}
	
	// Calculate load score for each peer
	for _, peer := range peers {
		loadScore := float64(peer.QueueDepth) * 0.3 +
			peer.LoadAverage * 0.3 +
			peer.CPUUsage * 0.2 +
			float64(100-min(100, int(peer.AvailableVRAM/1024/1024/1024*10))) * 0.2
		
		peer.Score = loadScore
	}
	
	// Sort by load score (lower is better)
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].Score < peers[j].Score
	})
	
	return peers[0]
}

func (s *Scheduler) selectLatencyAware(peers []*PeerMetrics) *PeerMetrics {
	if len(peers) == 0 {
		return nil
	}
	
	s.latencyMutex.RLock()
	defer s.latencyMutex.RUnlock()
	
	// Calculate combined score based on latency and load
	for _, peer := range peers {
		latencyScore := float64(100)
		if latency, exists := s.latencyMap[peer.PeerID]; exists {
			// Convert RTT to score (lower is better)
			latencyScore = float64(latency.RTT.Milliseconds())
			// Add packet loss penalty
			latencyScore += latency.PacketLoss * 1000
		}
		
		loadScore := float64(peer.QueueDepth)*10 + peer.LoadAverage*20
		
		// Combined score (lower is better)
		peer.Score = latencyScore*0.6 + loadScore*0.4
	}
	
	// Sort by combined score
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].Score < peers[j].Score
	})
	
	return peers[0]
}

func (s *Scheduler) selectCapabilityBased(peers []*PeerMetrics, criteria *SelectionCriteria) *PeerMetrics {
	if len(peers) == 0 {
		return nil
	}
	
	// Score based on capability match and performance
	for _, peer := range peers {
		capScore := float64(0)
		
		// Token generation speed
		if peer.TokensPerSecond > 0 {
			capScore += float64(peer.TokensPerSecond) / 10
		}
		
		// Context size bonus
		if peer.ContextSize > criteria.RequiredContext {
			capScore += float64(peer.ContextSize-criteria.RequiredContext) / 1000
		}
		
		// VRAM bonus
		if peer.AvailableVRAM > 0 {
			capScore += float64(peer.AvailableVRAM) / (1024 * 1024 * 1024) * 10
		}
		
		// Success rate
		if peer.RequestCount > 0 {
			successRate := float64(peer.RequestCount-peer.ErrorCount) / float64(peer.RequestCount)
			capScore += successRate * 50
		}
		
		peer.Score = capScore
	}
	
	// Sort by capability score (higher is better)
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].Score > peers[j].Score
	})
	
	return peers[0]
}

func (s *Scheduler) selectGeoProximity(peers []*PeerMetrics, criteria *SelectionCriteria) *PeerMetrics {
	if len(peers) == 0 {
		return nil
	}
	
	// Simple geo scoring (would need real geo distance calculation in production)
	for _, peer := range peers {
		if criteria.PreferredGeo != "" && peer.GeoLocation == criteria.PreferredGeo {
			peer.Score = 100
		} else {
			peer.Score = 50
		}
		
		// Combine with latency if available
		s.latencyMutex.RLock()
		if latency, exists := s.latencyMap[peer.PeerID]; exists {
			peer.Score -= float64(latency.RTT.Milliseconds()) / 10
		}
		s.latencyMutex.RUnlock()
	}
	
	// Sort by geo score (higher is better)
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].Score > peers[j].Score
	})
	
	return peers[0]
}

func (s *Scheduler) UpdatePeerMetrics(peerID peer.ID, metrics *PeerMetrics) {
	s.peersMutex.Lock()
	defer s.peersMutex.Unlock()
	
	metrics.PeerID = peerID
	metrics.LastHeartbeat = time.Now()
	s.peers[peerID] = metrics
}

func (s *Scheduler) RemovePeer(peerID peer.ID) {
	s.peersMutex.Lock()
	defer s.peersMutex.Unlock()
	
	delete(s.peers, peerID)
	
	s.latencyMutex.Lock()
	delete(s.latencyMap, peerID)
	s.latencyMutex.Unlock()
}

func (s *Scheduler) RecordRequestResult(peerID peer.ID, latency time.Duration, success bool) {
	s.peersMutex.Lock()
	defer s.peersMutex.Unlock()
	
	if peer, exists := s.peers[peerID]; exists {
		peer.RequestCount++
		if !success {
			peer.ErrorCount++
		}
		
		peer.TotalLatency += latency
		peer.AverageLatency = peer.TotalLatency / time.Duration(peer.RequestCount)
	}
}

func (s *Scheduler) metricsUpdateLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		// Update metrics from P2P node
		p2pPeers := s.p2pNode.GetPeers()
		
		s.peersMutex.Lock()
		for _, peerInfo := range p2pPeers {
			if metrics, exists := s.peers[peerInfo.ID]; exists {
				// Update from P2P info
				metrics.ModelID = peerInfo.ModelID
				metrics.ContextSize = peerInfo.ContextSize
				metrics.TokensPerSecond = peerInfo.TokensPerSecond
				metrics.GeoLocation = peerInfo.GeoLocation
				
				if peerInfo.Health != nil {
					metrics.QueueDepth = peerInfo.Health.QueueDepth
					metrics.LoadAverage = peerInfo.Health.LoadAverage
					metrics.AvailableVRAM = peerInfo.Health.AvailableVRAM
					metrics.CPUUsage = peerInfo.Health.CPUUsage
				}
			} else {
				// New peer
				s.peers[peerInfo.ID] = &PeerMetrics{
					PeerID:          peerInfo.ID,
					ModelID:         peerInfo.ModelID,
					ContextSize:     peerInfo.ContextSize,
					TokensPerSecond: peerInfo.TokensPerSecond,
					GeoLocation:     peerInfo.GeoLocation,
					LastHeartbeat:   time.Now(),
					Capabilities:    peerInfo.Capabilities,
				}
			}
		}
		s.peersMutex.Unlock()
	}
}

func (s *Scheduler) latencyProbeLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		s.peersMutex.RLock()
		peers := make([]peer.ID, 0, len(s.peers))
		for peerID := range s.peers {
			peers = append(peers, peerID)
		}
		s.peersMutex.RUnlock()
		
		// Measure latency to each peer
		for _, peerID := range peers {
			go s.measureLatency(peerID)
		}
	}
}

func (s *Scheduler) measureLatency(peerID peer.ID) {
	// Simple ping measurement using libp2p
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	start := time.Now()
	
	// Try to ping the peer
	stream, err := s.p2pNode.Host().NewStream(ctx, peerID, "/ping/1.0.0")
	if err != nil {
		s.logger.Debug("Failed to measure latency", zap.String("peer", peerID.String()), zap.Error(err))
		return
	}
	defer stream.Close()
	
	// Simple ping-pong
	if _, err := stream.Write([]byte("ping")); err != nil {
		return
	}
	
	buf := make([]byte, 4)
	if _, err := stream.Read(buf); err != nil {
		return
	}
	
	rtt := time.Since(start)
	
	s.latencyMutex.Lock()
	defer s.latencyMutex.Unlock()
	
	if metrics, exists := s.latencyMap[peerID]; exists {
		// Update with exponential moving average
		alpha := 0.3
		metrics.RTT = time.Duration(float64(metrics.RTT)*(1-alpha) + float64(rtt)*alpha)
		metrics.LastMeasured = time.Now()
		metrics.MeasurementCount++
	} else {
		s.latencyMap[peerID] = &LatencyMetrics{
			RTT:              rtt,
			LastMeasured:     time.Now(),
			MeasurementCount: 1,
		}
	}
}

func (s *Scheduler) healthCheckLoop() {
	ticker := time.NewTicker(s.config.HealthCheckRate)
	defer ticker.Stop()
	
	for range ticker.C {
		s.peersMutex.Lock()
		
		// Remove stale peers
		for peerID, metrics := range s.peers {
			if time.Since(metrics.LastHeartbeat) > 3*s.config.HealthCheckRate {
				delete(s.peers, peerID)
				s.logger.Info("Removed stale peer", zap.String("peer", peerID.String()))
			}
		}
		
		s.peersMutex.Unlock()
	}
}

func (s *Scheduler) GetPeerMetrics() map[peer.ID]*PeerMetrics {
	s.peersMutex.RLock()
	defer s.peersMutex.RUnlock()
	
	metrics := make(map[peer.ID]*PeerMetrics)
	for k, v := range s.peers {
		metrics[k] = v
	}
	return metrics
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func geoDistance(loc1, loc2 string) float64 {
	// Simplified geo distance calculation
	// In production, would use actual lat/lon coordinates
	if loc1 == loc2 {
		return 0
	}
	return math.MaxFloat64
}