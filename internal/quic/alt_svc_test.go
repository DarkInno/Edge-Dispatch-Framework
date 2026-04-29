package quic

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAltSvcMiddleware(t *testing.T) {
	mw := AltSvcMiddleware(":9443")
	if mw == nil {
		t.Fatal("AltSvcMiddleware returned nil")
	}
}

func TestAltSvcMiddlewareHTTPS(t *testing.T) {
	mw := AltSvcMiddleware(":9443")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ts := httptest.NewUnstartedServer(handler)
	ts.TLS = &tls.Config{}
	ts.StartTLS()
	defer ts.Close()

	client := ts.Client()
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	altSvc := resp.Header.Get(AltSvcHeader)
	if altSvc != `h3="::9443"` {
		t.Errorf("expected Alt-Svc header h3=\"::9443\", got %q", altSvc)
	}
}

func TestAltSvcMiddlewareNoTLS(t *testing.T) {
	mw := AltSvcMiddleware(":9443")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if altSvc := resp.Header.Get(AltSvcHeader); altSvc != "" {
		t.Errorf("expected no Alt-Svc header on plain HTTP, got %q", altSvc)
	}
}

func TestAltSvcMiddlewareEmptyAddr(t *testing.T) {
	mw := AltSvcMiddleware("")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ts := httptest.NewUnstartedServer(handler)
	ts.TLS = &tls.Config{}
	ts.StartTLS()
	defer ts.Close()

	client := ts.Client()
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if altSvc := resp.Header.Get(AltSvcHeader); altSvc != "" {
		t.Errorf("expected no Alt-Svc header with empty addr, got %q", altSvc)
	}
}

func TestAltSvcMiddlewarePassesThrough(t *testing.T) {
	mw := AltSvcMiddleware(":9443")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "foo")
		w.WriteHeader(http.StatusTeapot)
	}))

	ts := httptest.NewUnstartedServer(handler)
	ts.TLS = &tls.Config{}
	ts.StartTLS()
	defer ts.Close()

	client := ts.Client()
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("expected status 418, got %d", resp.StatusCode)
	}
	if custom := resp.Header.Get("X-Custom"); custom != "foo" {
		t.Errorf("expected X-Custom=foo, got %q", custom)
	}
}
