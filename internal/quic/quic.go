// Package quic provides an optional HTTP/3 listening scaffold (v0.4).
// To enable HTTP/3, import "github.com/quic-go/quic-go/http3" and
// construct an http3.Server in place of net/http.Server.
//
// Example usage:
//
//	import "github.com/quic-go/quic-go/http3"
//
//	ln, _ := quic.ListenAddr(listenAddr, tlsConf, nil)
//	srv := &http3.Server{Handler: mux, TLSConfig: tlsConf}
//	srv.Serve(ln)
//
// For production QUIC, ensure cert rotation, 0-RTT safety, and concurrency
// tuning (set GOMAXPROCS appropriately for UDP receive).
//
// Enable via build tag: go build -tags quic ./...
package quic

// Enabled reports whether the quic build tag is active.
var Enabled = false

// ListenAddr returns the QUIC listen address override.
const ListenAddr = ":9443"
