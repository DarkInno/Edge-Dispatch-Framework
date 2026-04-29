package quic

import "net/http"

// AltSvcHeader is the standard Alt-Svc header name.
const AltSvcHeader = "Alt-Svc"

// AltSvcMiddleware returns a middleware that adds the Alt-Svc header
// to HTTP responses, advertising HTTP/3 availability. This should be
// used on HTTP/1.1 and HTTP/2 endpoints so clients can upgrade.
func AltSvcMiddleware(quicAddr string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only advertise on HTTPS connections
			if r.TLS != nil && quicAddr != "" {
				w.Header().Set(AltSvcHeader, `h3=":`+quicAddr+`"`)
			}
			next.ServeHTTP(w, r)
		})
	}
}
