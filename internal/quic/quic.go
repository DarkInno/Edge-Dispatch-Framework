// Package quic provides optional HTTP/3 support via quic-go.
//
// Import "github.com/quic-go/quic-go/http3" and build with -tags quic
// to enable HTTP/3 listeners on gateway and edge agent services.
//
// Without the quic build tag, Server and Client are no-ops.
//
// Example:
//
//	cfg := quic.ServerConfig{
//	    Addr:      ":9443",
//	    Handler:   mux,
//	    TLSConfig: tlsCfg,
//	}
//	srv := quic.NewServer(cfg)
//	go srv.ListenAndServe()
package quic

// Enabled reports whether HTTP/3 support is available.
// Set to true by init() in withquic.go when built with -tags quic.
var Enabled bool

// DefaultListenAddr is the default QUIC listen address.
const DefaultListenAddr = ":9443"
