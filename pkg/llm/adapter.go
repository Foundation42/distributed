package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Foundation42/distributed/pkg/config"
	"go.uber.org/zap"
)

type Adapter interface {
	Generate(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error)
	Embeddings(ctx context.Context, texts []string) ([][]float32, error)
	GetModelInfo() *ModelInfo
	HealthCheck(ctx context.Context) error
	Close() error
}

type GenerateRequest struct {
	Prompt      string            `json:"prompt"`
	MaxTokens   int               `json:"max_tokens"`
	Temperature float32           `json:"temperature"`
	TopP        float32           `json:"top_p"`
	TopK        int               `json:"top_k"`
	Stop        []string          `json:"stop"`
	Stream      bool              `json:"stream"`
	Metadata    map[string]string `json:"metadata"`
}

type GenerateResponse struct {
	Text            string  `json:"text"`
	TokenID         int     `json:"token_id"`
	IsFinal         bool    `json:"is_final"`
	PromptTokens    int     `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TimeToFirstToken float64 `json:"time_to_first_token_ms"`
	TokensPerSecond float64 `json:"tokens_per_second"`
}

type ModelInfo struct {
	ModelID         string            `json:"model_id"`
	ContextSize     int               `json:"context_size"`
	TokensPerSecond int               `json:"tokens_per_second"`
	Capabilities    map[string]string `json:"capabilities"`
}

// LlamaCPPAdapter implements the Adapter interface for llama.cpp server
type LlamaCPPAdapter struct {
	config     *config.LLMConfig
	logger     *zap.Logger
	httpClient *http.Client
	baseURL    string
	modelInfo  *ModelInfo
	mu         sync.RWMutex
}

func NewLlamaCPPAdapter(cfg *config.LLMConfig, logger *zap.Logger) (*LlamaCPPAdapter, error) {
	adapter := &LlamaCPPAdapter{
		config:  cfg,
		logger:  logger,
		baseURL: strings.TrimSuffix(cfg.LlamaCPPURL, "/"),
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
		modelInfo: &ModelInfo{
			ModelID:         cfg.ModelID,
			ContextSize:     cfg.ContextSize,
			TokensPerSecond: cfg.TokensPerSecond,
			Capabilities:    cfg.Capabilities,
		},
	}
	
	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	if err := adapter.HealthCheck(ctx); err != nil {
		return nil, fmt.Errorf("failed to connect to llama.cpp server: %w", err)
	}
	
	logger.Info("LlamaCPP adapter initialized",
		zap.String("url", cfg.LlamaCPPURL),
		zap.String("model_id", cfg.ModelID),
	)
	
	return adapter, nil
}

func (a *LlamaCPPAdapter) Generate(ctx context.Context, req *GenerateRequest) (<-chan *GenerateResponse, error) {
	respChan := make(chan *GenerateResponse, 100)
	
	go func() {
		defer close(respChan)
		
		if req.Stream {
			a.generateStream(ctx, req, respChan)
		} else {
			a.generateComplete(ctx, req, respChan)
		}
	}()
	
	return respChan, nil
}

func (a *LlamaCPPAdapter) generateStream(ctx context.Context, req *GenerateRequest, respChan chan<- *GenerateResponse) {
	startTime := time.Now()
	
	// Prepare request body for llama.cpp
	body := map[string]interface{}{
		"prompt":      req.Prompt,
		"n_predict":   req.MaxTokens,
		"temperature": req.Temperature,
		"top_p":       req.TopP,
		"top_k":       req.TopK,
		"stop":        req.Stop,
		"stream":      true,
	}
	
	jsonBody, err := json.Marshal(body)
	if err != nil {
		a.logger.Error("Failed to marshal request", zap.Error(err))
		return
	}
	
	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.baseURL+"/completion", bytes.NewReader(jsonBody))
	if err != nil {
		a.logger.Error("Failed to create request", zap.Error(err))
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	
	// Send request
	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		a.logger.Error("Failed to send request", zap.Error(err))
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		a.logger.Error("Request failed", zap.Int("status", resp.StatusCode), zap.String("body", string(body)))
		return
	}
	
	// Parse streaming response
	decoder := json.NewDecoder(resp.Body)
	tokenCount := 0
	firstTokenTime := float64(0)
	
	for {
		var chunk map[string]interface{}
		if err := decoder.Decode(&chunk); err != nil {
			if err == io.EOF {
				break
			}
			a.logger.Error("Failed to decode chunk", zap.Error(err))
			break
		}
		
		// Extract token from chunk
		content, ok := chunk["content"].(string)
		if !ok {
			continue
		}
		
		tokenCount++
		if tokenCount == 1 {
			firstTokenTime = time.Since(startTime).Seconds() * 1000
		}
		
		// Check if this is the final token
		stop, _ := chunk["stop"].(bool)
		
		// Send response
		respChan <- &GenerateResponse{
			Text:             content,
			IsFinal:          stop,
			TimeToFirstToken: firstTokenTime,
			CompletionTokens: tokenCount,
		}
		
		if stop {
			break
		}
	}
	
	// Calculate tokens per second
	duration := time.Since(startTime).Seconds()
	if duration > 0 {
		tps := float64(tokenCount) / duration
		respChan <- &GenerateResponse{
			IsFinal:         true,
			CompletionTokens: tokenCount,
			TokensPerSecond: tps,
		}
	}
}

func (a *LlamaCPPAdapter) generateComplete(ctx context.Context, req *GenerateRequest, respChan chan<- *GenerateResponse) {
	startTime := time.Now()
	
	// Prepare request body
	body := map[string]interface{}{
		"prompt":      req.Prompt,
		"n_predict":   req.MaxTokens,
		"temperature": req.Temperature,
		"top_p":       req.TopP,
		"top_k":       req.TopK,
		"stop":        req.Stop,
		"stream":      false,
	}
	
	jsonBody, err := json.Marshal(body)
	if err != nil {
		a.logger.Error("Failed to marshal request", zap.Error(err))
		return
	}
	
	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.baseURL+"/completion", bytes.NewReader(jsonBody))
	if err != nil {
		a.logger.Error("Failed to create request", zap.Error(err))
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	
	// Send request
	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		a.logger.Error("Failed to send request", zap.Error(err))
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		a.logger.Error("Request failed", zap.Int("status", resp.StatusCode), zap.String("body", string(body)))
		return
	}
	
	// Parse response
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		a.logger.Error("Failed to decode response", zap.Error(err))
		return
	}
	
	content, _ := result["content"].(string)
	
	duration := time.Since(startTime).Seconds()
	tokenCount := len(strings.Fields(content)) // Approximate token count
	
	respChan <- &GenerateResponse{
		Text:             content,
		IsFinal:          true,
		CompletionTokens: tokenCount,
		TimeToFirstToken: duration * 1000,
		TokensPerSecond:  float64(tokenCount) / duration,
	}
}

func (a *LlamaCPPAdapter) Embeddings(ctx context.Context, texts []string) ([][]float32, error) {
	embeddings := make([][]float32, len(texts))
	
	for i, text := range texts {
		// Prepare request
		body := map[string]interface{}{
			"content": text,
		}
		
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
		
		// Create HTTP request
		httpReq, err := http.NewRequestWithContext(ctx, "POST", a.baseURL+"/embedding", bytes.NewReader(jsonBody))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		
		// Send request
		resp, err := a.httpClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("failed to send request: %w", err)
		}
		defer resp.Body.Close()
		
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, body)
		}
		
		// Parse response
		var result struct {
			Embedding []float32 `json:"embedding"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		
		embeddings[i] = result.Embedding
	}
	
	return embeddings, nil
}

func (a *LlamaCPPAdapter) GetModelInfo() *ModelInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.modelInfo
}

func (a *LlamaCPPAdapter) HealthCheck(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", a.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	
	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("health check failed with status %d: %s", resp.StatusCode, body)
	}
	
	// Try to get model info
	if err := a.updateModelInfo(ctx); err != nil {
		a.logger.Warn("Failed to update model info", zap.Error(err))
	}
	
	return nil
}

func (a *LlamaCPPAdapter) updateModelInfo(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", a.baseURL+"/props", nil)
	if err != nil {
		return err
	}
	
	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode == http.StatusOK {
		var props map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&props); err == nil {
			a.mu.Lock()
			defer a.mu.Unlock()
			
			if ctx, ok := props["n_ctx"].(float64); ok {
				a.modelInfo.ContextSize = int(ctx)
			}
		}
	}
	
	return nil
}

func (a *LlamaCPPAdapter) Close() error {
	// Nothing to clean up for HTTP adapter
	return nil
}