package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"net"
	"sync"
	"time"
)

var hmacPool = sync.Pool{
	New: func() any {
		return hmac.New(sha256.New, nil)
	},
}

// TokenPayload is the content signed into a dispatch token.
type TokenPayload struct {
	Key    string `json:"key"`
	Exp    int64  `json:"exp"`
	IPPrefix string `json:"ip_prefix,omitempty"`
}

// Signer creates and verifies HMAC-signed tokens.
type Signer struct {
	secret []byte
}

var sha256Pool = sync.Pool{
	New: func() any { return sha256.New() },
}

// NewSigner creates a new Signer with the given secret.
func NewSigner(secret string) *Signer {
	return &Signer{secret: []byte(secret)}
}

// Sign creates a signed token for the given payload.
// Format: base64url(payload) + "." + base64url(hmac_signature)
func (s *Signer) Sign(p TokenPayload) (string, error) {
	if p.Exp == 0 {
		p.Exp = time.Now().Add(5 * time.Minute).Unix()
	}
	payloadBytes, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payloadBytes)

	inner := sha256Pool.Get().(hash.Hash)
	inner.Reset()
	mac := hmac.New(func() hash.Hash { return inner }, s.secret)
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	sha256Pool.Put(inner)

	return encoded + "." + sig, nil
}

// Verify checks a token's signature and expiration.
func (s *Signer) Verify(token string) (*TokenPayload, error) {
	parts := splitToken(token)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid token format")
	}
	encoded, sig := parts[0], parts[1]

	inner := sha256Pool.Get().(hash.Hash)
	inner.Reset()
	mac := hmac.New(func() hash.Hash { return inner }, s.secret)
	mac.Write([]byte(encoded))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	sha256Pool.Put(inner)

	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return nil, fmt.Errorf("invalid signature")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var p TokenPayload
	if err := json.Unmarshal(payloadBytes, &p); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}
	if time.Now().Unix() > p.Exp {
		return nil, fmt.Errorf("token expired")
	}
	return &p, nil
}

// MatchesIP returns true if ip falls within the IPPrefix CIDR.
// Returns true if IPPrefix is empty (no IP restriction).
func (p *TokenPayload) MatchesIP(ip string) (bool, error) {
	if p.IPPrefix == "" {
		return true, nil
	}
	_, cidr, err := net.ParseCIDR(p.IPPrefix)
	if err != nil {
		return false, fmt.Errorf("invalid ip_prefix in token: %w", err)
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false, fmt.Errorf("invalid client ip: %q", ip)
	}
	return cidr.Contains(parsed), nil
}

// VerifyWithIP validates the token and checks IP binding.
func (s *Signer) VerifyWithIP(token, clientIP string) (*TokenPayload, error) {
	p, err := s.Verify(token)
	if err != nil {
		return nil, err
	}
	ok, err := p.MatchesIP(clientIP)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("client ip %q does not match token ip_prefix %q", clientIP, p.IPPrefix)
	}
	return p, nil
}

// ComputeIPPrefix returns a CIDR prefix for the given IP address.
// Uses /24 for IPv4 and /64 for IPv6.
func ComputeIPPrefix(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if v4 := parsed.To4(); v4 != nil {
		mask := net.CIDRMask(24, 32)
		return (&net.IPNet{IP: v4.Mask(mask), Mask: mask}).String()
	}
	mask := net.CIDRMask(64, 128)
	return (&net.IPNet{IP: parsed.Mask(mask), Mask: mask}).String()
}

func splitToken(token string) []string {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			return []string{token[:i], token[i+1:]}
		}
	}
	return []string{token}
}
