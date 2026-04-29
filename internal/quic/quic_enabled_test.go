//go:build quic

package quic

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

func TestEnabledWithQuic(t *testing.T) {
	if !Enabled {
		t.Error("Expected Enabled=true with -tags quic")
	}
}

func generateTestTLSConfig(t *testing.T) *tls.Config {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	cert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

func TestNewServer(t *testing.T) {
	tlsCfg := generateTestTLSConfig(t)
	srv := NewServer(ServerConfig{
		Addr:      ":0",
		Handler:   http.NewServeMux(),
		TLSConfig: tlsCfg,
	})
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	if srv.srv == nil {
		t.Fatal("server internal http3.Server is nil")
	}

	// Start and immediately close
	go func() {
		srv.ListenAndServe()
	}()
	time.Sleep(50 * time.Millisecond)

	if err := srv.Close(); err != nil {
		t.Errorf("Close error: %v", err)
	}
}

func TestNewServerNoTLS(t *testing.T) {
	srv := NewServer(ServerConfig{
		Addr: ":0",
	})
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	// Should warn but not panic
	if err := srv.ListenAndServe(); err == nil {
		t.Error("expected error without TLS config")
	}
}

func TestCloseWithoutListen(t *testing.T) {
	tlsCfg := generateTestTLSConfig(t)
	srv := NewServer(ServerConfig{
		Addr:      ":0",
		Handler:   http.NewServeMux(),
		TLSConfig: tlsCfg,
	})
	if err := srv.Close(); err != nil {
		t.Errorf("Close without ListenAndServe should not error: %v", err)
	}
}

func TestServerAddr(t *testing.T) {
	tlsCfg := generateTestTLSConfig(t)
	srv := NewServer(ServerConfig{
		Addr:      ":9999",
		Handler:   http.NewServeMux(),
		TLSConfig: tlsCfg,
	})
	if addr := srv.Addr(); addr != ":9999" {
		t.Errorf("expected addr :9999, got %q", addr)
	}
}

func TestCloseMultipleTimes(t *testing.T) {
	tlsCfg := generateTestTLSConfig(t)
	srv := NewServer(ServerConfig{
		Addr:      ":0",
		Handler:   http.NewServeMux(),
		TLSConfig: tlsCfg,
	})
	go func() {
		srv.ListenAndServe()
	}()
	time.Sleep(50 * time.Millisecond)

	if err := srv.Close(); err != nil {
		t.Errorf("first Close error: %v", err)
	}
	// Second close should be idempotent
	if err := srv.Close(); err != nil {
		t.Errorf("second Close error: %v", err)
	}
}

func TestListenAndServeInvalidAddr(t *testing.T) {
	tlsCfg := generateTestTLSConfig(t)
	srv := NewServer(ServerConfig{
		Addr:      ":99999", // invalid port
		Handler:   http.NewServeMux(),
		TLSConfig: tlsCfg,
	})
	err := srv.ListenAndServe()
	if err == nil {
		t.Error("expected error for invalid address")
	}
}

func TestServerConfigDefaultValues(t *testing.T) {
	tlsCfg := generateTestTLSConfig(t)
	cfg := ServerConfig{
		Addr:      ":0",
		Handler:   http.NewServeMux(),
		TLSConfig: tlsCfg,
	}
	srv := NewServer(cfg)
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	// Verify the internal http3.Server has the correct handler
	if srv.srv.Handler == nil {
		t.Error("expected non-nil handler on http3.Server")
	}
}

func TestHTTP3Roundtrip(t *testing.T) {
	tlsCfg := generateTestTLSConfig(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	})

	srv := NewServer(ServerConfig{
		Addr:      "127.0.0.1:0",
		Handler:   mux,
		TLSConfig: tlsCfg,
	})
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}

	go func() {
		srv.ListenAndServe()
	}()
	time.Sleep(100 * time.Millisecond)

	defer srv.Close()

	// Use HTTP/3 client from quic-go/http3
	roundTripper := &http3.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	defer roundTripper.Close()

	client := &http.Client{
		Transport: roundTripper,
	}

	addr := srv.Addr()
	resp, err := client.Get("https://127.0.0.1" + addr + "/ping")
	if err != nil {
		t.Fatalf("HTTP/3 GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

