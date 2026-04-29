package controlplane

import "sync/atomic"

type policyData struct {
	blockedIPs   map[string]bool
	blockedNodes map[string]bool
}

type Policy struct {
	data atomic.Pointer[policyData]
}

func NewPolicy() *Policy {
	p := &Policy{}
	p.data.Store(&policyData{
		blockedIPs:   make(map[string]bool),
		blockedNodes: make(map[string]bool),
	})
	return p
}

func (p *Policy) IsBlocked(ip string) bool {
	return p.data.Load().blockedIPs[ip]
}

func (p *Policy) BlockIP(ip string) {
	old := p.data.Load()
	newData := &policyData{
		blockedIPs:   copyMap(old.blockedIPs),
		blockedNodes: old.blockedNodes,
	}
	newData.blockedIPs[ip] = true
	p.data.Store(newData)
}

func (p *Policy) UnblockIP(ip string) {
	old := p.data.Load()
	newData := &policyData{
		blockedIPs:   copyMap(old.blockedIPs),
		blockedNodes: old.blockedNodes,
	}
	delete(newData.blockedIPs, ip)
	p.data.Store(newData)
}

func (p *Policy) IsNodeBlocked(nodeID string) bool {
	return p.data.Load().blockedNodes[nodeID]
}

func (p *Policy) BlockNode(nodeID string) {
	old := p.data.Load()
	newData := &policyData{
		blockedIPs:   old.blockedIPs,
		blockedNodes: copyMap(old.blockedNodes),
	}
	newData.blockedNodes[nodeID] = true
	p.data.Store(newData)
}

func (p *Policy) UnblockNode(nodeID string) {
	old := p.data.Load()
	newData := &policyData{
		blockedIPs:   old.blockedIPs,
		blockedNodes: copyMap(old.blockedNodes),
	}
	delete(newData.blockedNodes, nodeID)
	p.data.Store(newData)
}

func copyMap(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
