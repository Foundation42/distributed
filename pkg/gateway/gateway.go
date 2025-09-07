package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Foundation42/distributed/pkg/config"
	"github.com/Foundation42/distributed/pkg/llm"
	"github.com/Foundation42/distributed/pkg/p2p"
	"github.com/Foundation42/distributed/pkg/scheduler"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

type Gateway struct {
	config    *config.GatewayConfig
	logger    *zap.Logger
	p2pNode   *p2p.Node
	scheduler *scheduler.Scheduler
	llm       llm.Adapter
	server    *http.Server
	router    *mux.Router
	
	// Metrics
	requestCount    prometheus.Counter
	requestDuration prometheus.Histogram
	errorCount      prometheus.Counter
}

type GenerateRequest struct {
	Prompt      string            `json:"prompt"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature float32           `json:"temperature,omitempty"`
	TopP        float32           `json:"top_p,omitempty"`
	TopK        int               `json:"top_k,omitempty"`
	Stream      bool              `json:"stream,omitempty"`
	ModelID     string            `json:"model,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type GenerateResponse struct {
	Text            string  `json:"text,omitempty"`
	Tokens          []Token `json:"tokens,omitempty"`
	PromptTokens    int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	TotalTokens     int     `json:"total_tokens,omitempty"`
	TimeToFirstToken float64 `json:"time_to_first_token_ms,omitempty"`
	TokensPerSecond float64 `json:"tokens_per_second,omitempty"`
	Error           string  `json:"error,omitempty"`
}

type Token struct {
	Text    string `json:"text"`
	TokenID int    `json:"token_id"`
}

type EmbeddingsRequest struct {
	Input   interface{} `json:"input"`
	Model   string      `json:"model,omitempty"`
}

type EmbeddingsResponse struct {
	Data  []EmbeddingData `json:"data"`
	Model string          `json:"model"`
	Usage Usage           `json:"usage"`
}

type EmbeddingData struct {
	Object    string    `json:"object"`
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type Usage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type HealthResponse struct {
	Status       string                 `json:"status"`
	PeerID       string                 `json:"peer_id"`
	ConnectedPeers int                  `json:"connected_peers"`
	ModelInfo    interface{}            `json:"model_info,omitempty"`
	Metrics      map[string]interface{} `json:"metrics"`
}

func NewGateway(cfg *config.GatewayConfig, p2pNode *p2p.Node, scheduler *scheduler.Scheduler, llm llm.Adapter, logger *zap.Logger) *Gateway {
	g := &Gateway{
		config:    cfg,
		logger:    logger,
		p2pNode:   p2pNode,
		scheduler: scheduler,
		llm:       llm,
		router:    mux.NewRouter(),
	}
	
	// Initialize metrics
	g.requestCount = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Total number of gateway requests",
	})
	
	g.requestDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "gateway_request_duration_seconds",
		Help:    "Request duration in seconds",
		Buckets: prometheus.DefBuckets,
	})
	
	g.errorCount = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gateway_errors_total",
		Help: "Total number of gateway errors",
	})
	
	prometheus.MustRegister(g.requestCount, g.requestDuration, g.errorCount)
	
	// Setup routes
	g.setupRoutes()
	
	return g
}

func (g *Gateway) setupRoutes() {
	// API routes
	api := g.router.PathPrefix("/v1").Subrouter()
	api.HandleFunc("/completions", g.handleGenerate).Methods("POST")
	api.HandleFunc("/chat/completions", g.handleChatCompletions).Methods("POST")
	api.HandleFunc("/embeddings", g.handleEmbeddings).Methods("POST")
	api.HandleFunc("/models", g.handleModels).Methods("GET")
	
	// Simple generate endpoint
	g.router.HandleFunc("/generate", g.handleGenerate).Methods("POST")
	
	// Health and metrics
	g.router.HandleFunc("/health", g.handleHealth).Methods("GET")
	g.router.Handle("/metrics", promhttp.Handler())
	
	// Peer management
	g.router.HandleFunc("/peers", g.handlePeers).Methods("GET")
	g.router.HandleFunc("/peers/metrics", g.handlePeerMetrics).Methods("GET")
	
	// Static info
	g.router.HandleFunc("/", g.handleRoot).Methods("GET")
}

func (g *Gateway) Start() error {
	g.server = &http.Server{
		Addr:         g.config.ListenAddr,
		Handler:      g.router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}
	
	g.logger.Info("Starting HTTP gateway", zap.String("addr", g.config.ListenAddr))
	
	if g.config.EnableTLS {
		return g.server.ListenAndServeTLS(g.config.CertFile, g.config.KeyFile)
	}
	return g.server.ListenAndServe()
}

func (g *Gateway) Stop() error {
	if g.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return g.server.Shutdown(ctx)
	}
	return nil
}

func (g *Gateway) handleGenerate(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	g.requestCount.Inc()
	defer func() {
		g.requestDuration.Observe(time.Since(start).Seconds())
	}()
	
	var req GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		g.errorCount.Inc()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	// Set defaults
	if req.MaxTokens == 0 {
		req.MaxTokens = 128
	}
	if req.Temperature == 0 {
		req.Temperature = 0.7
	}
	
	// Use local LLM if available
	if g.llm != nil {
		g.handleLocalGenerate(w, r, &req)
		return
	}
	
	// Route to peer
	g.handleDistributedGenerate(w, r, &req)
}

func (g *Gateway) handleLocalGenerate(w http.ResponseWriter, r *http.Request, req *GenerateRequest) {
	ctx := r.Context()
	
	llmReq := &llm.GenerateRequest{
		Prompt:      req.Prompt,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		TopK:        req.TopK,
		Stream:      req.Stream,
		Metadata:    req.Metadata,
	}
	
	respChan, err := g.llm.Generate(ctx, llmReq)
	if err != nil {
		g.errorCount.Inc()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	if req.Stream {
		g.streamResponse(w, respChan)
	} else {
		g.completeResponse(w, respChan)
	}
}

func (g *Gateway) handleDistributedGenerate(w http.ResponseWriter, r *http.Request, req *GenerateRequest) {
	// Select peer
	criteria := &scheduler.SelectionCriteria{
		ModelID:         req.ModelID,
		RequiredContext: 2048, // Default minimum
	}
	
	peer, err := g.scheduler.SelectPeer(r.Context(), criteria)
	if err != nil {
		g.errorCount.Inc()
		http.Error(w, fmt.Sprintf("No peers available: %v", err), http.StatusServiceUnavailable)
		return
	}
	
	// TODO: Forward request to selected peer via P2P network
	// For now, return error
	http.Error(w, "Distributed generation not yet implemented", http.StatusNotImplemented)
}

func (g *Gateway) streamResponse(w http.ResponseWriter, respChan <-chan *llm.GenerateResponse) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}
	
	for resp := range respChan {
		data, err := json.Marshal(map[string]interface{}{
			"text":      resp.Text,
			"is_final":  resp.IsFinal,
		})
		if err != nil {
			continue
		}
		
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		
		if resp.IsFinal {
			break
		}
	}
	
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (g *Gateway) completeResponse(w http.ResponseWriter, respChan <-chan *llm.GenerateResponse) {
	var fullText string
	var stats *llm.GenerateResponse
	
	for resp := range respChan {
		fullText += resp.Text
		if resp.IsFinal {
			stats = resp
		}
	}
	
	response := GenerateResponse{
		Text:             fullText,
		CompletionTokens: stats.CompletionTokens,
		TimeToFirstToken: stats.TimeToFirstToken,
		TokensPerSecond:  stats.TokensPerSecond,
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (g *Gateway) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// OpenAI-compatible chat endpoint
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens   int     `json:"max_tokens,omitempty"`
		Temperature float32 `json:"temperature,omitempty"`
		Stream      bool    `json:"stream,omitempty"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	// Convert chat format to simple prompt
	var prompt string
	for _, msg := range req.Messages {
		prompt += fmt.Sprintf("%s: %s\n", msg.Role, msg.Content)
	}
	
	genReq := &GenerateRequest{
		Prompt:      prompt,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      req.Stream,
		ModelID:     req.Model,
	}
	
	g.handleGenerate(w, r.WithContext(r.Context()), genReq)
}

func (g *Gateway) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req EmbeddingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	// Extract texts from input
	var texts []string
	switch v := req.Input.(type) {
	case string:
		texts = []string{v}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				texts = append(texts, s)
			}
		}
	default:
		http.Error(w, "Invalid input format", http.StatusBadRequest)
		return
	}
	
	if g.llm == nil {
		http.Error(w, "Embeddings not available", http.StatusServiceUnavailable)
		return
	}
	
	embeddings, err := g.llm.Embeddings(r.Context(), texts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	// Format response
	response := EmbeddingsResponse{
		Model: req.Model,
		Data:  make([]EmbeddingData, len(embeddings)),
		Usage: Usage{
			PromptTokens: len(texts) * 10, // Approximate
			TotalTokens:  len(texts) * 10,
		},
	}
	
	for i, emb := range embeddings {
		response.Data[i] = EmbeddingData{
			Object:    "embedding",
			Embedding: emb,
			Index:     i,
		}
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	models := []map[string]interface{}{}
	
	if g.llm != nil {
		info := g.llm.GetModelInfo()
		models = append(models, map[string]interface{}{
			"id":      info.ModelID,
			"object":  "model",
			"created": time.Now().Unix(),
			"owned_by": "local",
			"context_size": info.ContextSize,
		})
	}
	
	// Add peer models
	for _, peer := range g.scheduler.GetPeerMetrics() {
		models = append(models, map[string]interface{}{
			"id":       peer.ModelID,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": fmt.Sprintf("peer:%s", peer.PeerID.String()[:8]),
			"context_size": peer.ContextSize,
		})
	}
	
	response := map[string]interface{}{
		"object": "list",
		"data":   models,
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	peers := g.p2pNode.GetPeers()
	
	health := HealthResponse{
		Status:         "healthy",
		PeerID:         g.p2pNode.Host().ID().String(),
		ConnectedPeers: len(peers),
		Metrics: map[string]interface{}{
			"requests_total": g.requestCount,
			"errors_total":   g.errorCount,
		},
	}
	
	if g.llm != nil {
		health.ModelInfo = g.llm.GetModelInfo()
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}

func (g *Gateway) handlePeers(w http.ResponseWriter, r *http.Request) {
	peers := g.p2pNode.GetPeers()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(peers)
}

func (g *Gateway) handlePeerMetrics(w http.ResponseWriter, r *http.Request) {
	metrics := g.scheduler.GetPeerMetrics()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics)
}

func (g *Gateway) handleRoot(w http.ResponseWriter, r *http.Request) {
	info := map[string]interface{}{
		"name":    "Distributed Platform Gateway",
		"version": "0.1.0",
		"peer_id": g.p2pNode.Host().ID().String(),
		"endpoints": map[string]string{
			"generate":    "/generate",
			"completions": "/v1/completions",
			"chat":        "/v1/chat/completions",
			"embeddings":  "/v1/embeddings",
			"models":      "/v1/models",
			"health":      "/health",
			"metrics":     "/metrics",
			"peers":       "/peers",
		},
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}