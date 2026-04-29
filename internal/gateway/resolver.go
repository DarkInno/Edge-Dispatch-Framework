package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// ControlPlaneClient interfaces with the control plane API.
type ControlPlaneClient struct {
	baseURL    string
	authToken  string
	httpClient *http.Client
	logger     *slog.Logger
	cache      sync.Map // nodeID -> cachedNodeInfo
}

// nodeInfoCache caches node information.
type nodeInfoCache struct {
	endpoint   string
	isPublic   bool
	expireAt   time.Time
}

// NodeInfo represents node information from control plane.
type NodeInfo struct {
	NodeID       string   `json:"node_id"`
	Endpoints    []EndpointInfo `json:"endpoints"`
	InboundReachable bool `json:"inbound_reachable"`
}

// EndpointInfo represents a node endpoint.
type EndpointInfo struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
}

// DispatchRequest is sent to the control plane for scheduling.
type DispatchRequest struct {
	Client   ClientInfo   `json:"client"`
	Resource ResourceInfo `json:"resource"`
	Hints    HintsInfo    `json:"hints"`
}

// ClientInfo contains client metadata.
type ClientInfo struct {
	IP string `json:"ip"`
}

// ResourceInfo describes the requested resource.
type ResourceInfo struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// HintsInfo provides scheduling hints.
type HintsInfo struct {
	NeedRange bool `json:"need_range,omitempty"`
}

// DispatchResponse is returned by the control plane.
type DispatchResponse struct {
	Candidates []CandidateInfo `json:"candidates"`
}

// CandidateInfo represents a scheduled edge node.
type CandidateInfo struct {
	ID       string `json:"id"`
	Endpoint string `json:"endpoint"`
	Weight   int    `json:"weight"`
}

// NewControlPlaneClient creates a new control plane client.
func NewControlPlaneClient(baseURL, authToken string, logger *slog.Logger) *ControlPlaneClient {
	if logger == nil {
		logger = slog.Default()
	}
	return &ControlPlaneClient{
		baseURL:   baseURL,
		authToken: authToken,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		logger: logger,
	}
}

// GetNodeEndpoint returns the endpoint for a node and whether it's public.
func (c *ControlPlaneClient) GetNodeEndpoint(nodeID string) (string, bool, error) {
	// Check cache first
	if cached, ok := c.cache.Load(nodeID); ok {
		info := cached.(nodeInfoCache)
		if time.Now().Before(info.expireAt) {
			return info.endpoint, info.isPublic, nil
		}
	}

	// Fetch from control plane
	url := fmt.Sprintf("%s/v1/nodes/%s", c.baseURL, nodeID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", false, fmt.Errorf("create request: %w", err)
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("fetch node: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("node not found: %s", nodeID)
	}

	var nodeInfo NodeInfo
	if err := json.NewDecoder(resp.Body).Decode(&nodeInfo); err != nil {
		return "", false, fmt.Errorf("decode node: %w", err)
	}

	// Build endpoint URL
	endpoint := ""
	isPublic := nodeInfo.InboundReachable

	if isPublic && len(nodeInfo.Endpoints) > 0 {
		ep := nodeInfo.Endpoints[0]
		endpoint = fmt.Sprintf("%s://%s:%d", ep.Scheme, ep.Host, ep.Port)
	}

	// Cache result
	c.cache.Store(nodeID, nodeInfoCache{
		endpoint: endpoint,
		isPublic: isPublic,
		expireAt: time.Now().Add(30 * time.Second),
	})

	return endpoint, isPublic, nil
}

// GetBestNode returns the best node ID for a resource request.
func (c *ControlPlaneClient) GetBestNode(resourceKey string, clientIP string) (string, error) {
	req := DispatchRequest{
		Client:   ClientInfo{IP: clientIP},
		Resource: ResourceInfo{Type: "object", Key: resourceKey},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/v1/dispatch/resolve", c.baseURL)
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.authToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("dispatch request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("dispatch failed: %d", resp.StatusCode)
	}

	var dispatchResp DispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&dispatchResp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(dispatchResp.Candidates) == 0 {
		return "", fmt.Errorf("no candidates available")
	}

	return dispatchResp.Candidates[0].ID, nil
}

// InvalidateCache removes a node from the cache.
func (c *ControlPlaneClient) InvalidateCache(nodeID string) {
	c.cache.Delete(nodeID)
}
