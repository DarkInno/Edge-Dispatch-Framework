package controlplane

import "sync"

type Policy struct {
	mu           sync.RWMutex
	blockedIPs   map[string]bool
	blockedNodes map[string]bool
}

func NewPolicy() *Policy {
	return &Policy{
		blockedIPs:   make(map[string]bool),
		blockedNodes: make(map[string]bool),
	}
}

func (p *Policy) IsBlocked(ip string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.blockedIPs[ip]
}

func (p *Policy) BlockIP(ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blockedIPs[ip] = true
}

func (p *Policy) UnblockIP(ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.blockedIPs, ip)
}

func (p *Policy) IsNodeBlocked(nodeID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.blockedNodes[nodeID]
}

func (p *Policy) BlockNode(nodeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blockedNodes[nodeID] = true
}

func (p *Policy) UnblockNode(nodeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.blockedNodes, nodeID)
}
