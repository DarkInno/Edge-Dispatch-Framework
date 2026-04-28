package controlplane

import (
	"sync"
	"testing"
)

func TestPolicyBlockUnblockIP(t *testing.T) {
	p := NewPolicy()

	if p.IsBlocked("1.2.3.4") {
		t.Fatal("IP should not be blocked initially")
	}

	p.BlockIP("1.2.3.4")
	if !p.IsBlocked("1.2.3.4") {
		t.Fatal("IP should be blocked")
	}

	// Other IPs should not be affected
	if p.IsBlocked("5.6.7.8") {
		t.Fatal("other IP should not be blocked")
	}

	p.UnblockIP("1.2.3.4")
	if p.IsBlocked("1.2.3.4") {
		t.Fatal("IP should be unblocked")
	}
}

func TestPolicyBlockUnblockNode(t *testing.T) {
	p := NewPolicy()

	if p.IsNodeBlocked("node-1") {
		t.Fatal("node should not be blocked initially")
	}

	p.BlockNode("node-1")
	if !p.IsNodeBlocked("node-1") {
		t.Fatal("node should be blocked")
	}

	p.UnblockNode("node-1")
	if p.IsNodeBlocked("node-1") {
		t.Fatal("node should be unblocked")
	}
}

func TestPolicyMultipleIPs(t *testing.T) {
	p := NewPolicy()
	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}

	for _, ip := range ips {
		p.BlockIP(ip)
	}

	for _, ip := range ips {
		if !p.IsBlocked(ip) {
			t.Errorf("IP %s should be blocked", ip)
		}
	}

	p.UnblockIP("2.2.2.2")
	if p.IsBlocked("2.2.2.2") {
		t.Fatal("2.2.2.2 should be unblocked")
	}
	if !p.IsBlocked("1.1.1.1") || !p.IsBlocked("3.3.3.3") {
		t.Fatal("other IPs should remain blocked")
	}
}

func TestPolicyConcurrent(t *testing.T) {
	p := NewPolicy()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			p.BlockIP("ip-" + string(rune(id)))
		}(i)
		go func(id int) {
			defer wg.Done()
			p.IsBlocked("ip-" + string(rune(id)))
		}(i)
	}
	wg.Wait()

	if p.IsBlocked("ip-\x00") {
	}
}
