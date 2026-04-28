package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

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
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encoded + "." + sig, nil
}

// Verify checks a token's signature and expiration.
func (s *Signer) Verify(token string) (*TokenPayload, error) {
	parts := splitToken(token)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid token format")
	}
	encoded, sig := parts[0], parts[1]
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(encoded))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
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

func splitToken(token string) []string {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			return []string{token[:i], token[i+1:]}
		}
	}
	return []string{token}
}
