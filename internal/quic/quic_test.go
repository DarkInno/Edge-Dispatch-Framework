//go:build !quic

package quic

import (
	"crypto/tls"
	"testing"
)

func TestEnabledStub(t *testing.T) {
	// Enabled should be false without the quic build tag
	if Enabled {
		t.Error("Expected Enabled=false without -tags quic")
	}
}

func TestNewServerStub(t *testing.T) {
	srv := NewServer(ServerConfig{
		Addr: ":9443",
	})
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	if err := srv.ListenAndServe(); err == nil {
		t.Error("expected error from stub ListenAndServe")
	}
	if err := srv.Close(); err != nil {
		t.Errorf("Close should not error: %v", err)
	}
	if addr := srv.Addr(); addr != "" {
		t.Errorf("expected empty addr from stub, got %q", addr)
	}
}

func TestStubCloseMultipleTimes(t *testing.T) {
	srv := NewServer(ServerConfig{})
	for i := 0; i < 3; i++ {
		if err := srv.Close(); err != nil {
			t.Errorf("Close iteration %d should not error: %v", i, err)
		}
	}
}

func TestStubWithTLSConfig(t *testing.T) {
	// Stub should ignore TLS config gracefully
	srv := NewServer(ServerConfig{
		Addr:      ":9443",
		TLSConfig: &tls.Config{},
	})
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	if err := srv.ListenAndServe(); err == nil {
		t.Error("expected error from stub ListenAndServe even with TLS config")
	}
}
