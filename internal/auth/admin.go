package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// AdminAuth provides HMAC-based authentication for internal admin APIs.
type AdminAuth struct {
	mu         sync.RWMutex
	accessKeys map[string]string // accessKey -> secretKey
	nonceTTL   time.Duration
	clockSkew  time.Duration
}

// NonceCache stores used nonces to prevent replay attacks.
type NonceCache struct {
	mu     sync.Mutex
	cache  map[string]time.Time
	maxAge time.Duration
}

// NewNonceCache creates a new nonce cache.
func NewNonceCache(maxAge time.Duration) *NonceCache {
	nc := &NonceCache{
		cache:  make(map[string]time.Time),
		maxAge: maxAge,
	}
	go nc.cleanupLoop()
	return nc
}

func (nc *NonceCache) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		nc.mu.Lock()
		now := time.Now()
		for k, v := range nc.cache {
			if now.Sub(v) > nc.maxAge {
				delete(nc.cache, k)
			}
		}
		nc.mu.Unlock()
	}
}

func (nc *NonceCache) CheckAndStore(nonce string) bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	if _, exists := nc.cache[nonce]; exists {
		return false
	}
	nc.cache[nonce] = time.Now()
	return true
}

// NewAdminAuth creates a new admin auth from an access key and secret key pair.
func NewAdminAuth(accessKey, secretKey string, nonceTTL, clockSkew time.Duration) *AdminAuth {
	a := &AdminAuth{
		accessKeys: make(map[string]string),
		nonceTTL:   nonceTTL,
		clockSkew:  clockSkew,
	}
	if accessKey != "" && secretKey != "" {
		a.accessKeys[accessKey] = secretKey
	}
	return a
}

// AddKey adds an access key / secret key pair.
func (a *AdminAuth) AddKey(accessKey, secretKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accessKeys[accessKey] = secretKey
}

// IsEnabled returns whether admin auth is configured.
func (a *AdminAuth) IsEnabled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.accessKeys) > 0
}

// Verify verifies the HMAC signature from the request headers.
// Required headers:
//   X-Admin-KeyId: the access key
//   X-Admin-Timestamp: Unix timestamp
//   X-Admin-Nonce: random nonce
//   X-Admin-Signature: HMAC-SHA256 signature of (keyId + timestamp + nonce)
func (a *AdminAuth) Verify(r *http.Request, nonceCache *NonceCache) (string, error) {
	keyID := r.Header.Get("X-Admin-KeyId")
	timestampStr := r.Header.Get("X-Admin-Timestamp")
	nonce := r.Header.Get("X-Admin-Nonce")
	signature := r.Header.Get("X-Admin-Signature")

	if keyID == "" || timestampStr == "" || nonce == "" || signature == "" {
		return "", fmt.Errorf("missing admin auth headers")
	}

	ts, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid timestamp")
	}

	now := time.Now().Unix()
	skew := int64(a.clockSkew.Seconds())
	if ts < now-skew || ts > now+skew {
		return "", fmt.Errorf("timestamp out of range")
	}

	if !nonceCache.CheckAndStore(nonce) {
		return "", fmt.Errorf("nonce already used (replay attack)")
	}

	a.mu.RLock()
	secretKey, ok := a.accessKeys[keyID]
	a.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("invalid access key")
	}

	expectedSig := computeHMAC(secretKey, keyID, timestampStr, nonce)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expectedSig)) != 1 {
		return "", fmt.Errorf("invalid signature")
	}

	return keyID, nil
}

func computeHMAC(secret, keyID, timestamp, nonce string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	msg := keyID + "|" + timestamp + "|" + nonce
	mac.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// AdminAuthMiddleware returns an HTTP middleware that enforces admin auth.
func (a *AdminAuth) Middleware(nonceCache *NonceCache) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !a.IsEnabled() {
				next.ServeHTTP(w, r)
				return
			}
			_, err := a.Verify(r, nonceCache)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"code":"ADMIN_UNAUTHORIZED","message":"admin auth failed: ` + err.Error() + `"}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ─── JWT Session ───────────────────────────────────────────────

// JWTClaims represents the JWT payload for admin sessions.
type JWTClaims struct {
	UserID    string `json:"user_id"`
	TenantID  string `json:"tenant_id,omitempty"`
	Email     string `json:"email"`
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
}

// JWTSession manages JWT token creation and verification.
type JWTSession struct {
	mu          sync.RWMutex
	secret      string
	expirySeconds int
}

// NewJWTSession creates a new JWT session manager.
func NewJWTSession(secret string, expirySeconds int) *JWTSession {
	return &JWTSession{
		secret:        secret,
		expirySeconds: expirySeconds,
	}
}

// Sign creates a new JWT token.
func (j *JWTSession) Sign(userID, tenantID, email string) (string, int64, error) {
	now := time.Now().Unix()
	exp := now + int64(j.expirySeconds)

	claims := JWTClaims{
		UserID:    userID,
		TenantID:  tenantID,
		Email:     email,
		ExpiresAt: exp,
		IssuedAt:  now,
	}

	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(claims)

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	signingInput := headerB64 + "." + payloadB64
	mac := hmac.New(sha256.New, []byte(j.secret))
	mac.Write([]byte(signingInput))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return signingInput + "." + signature, exp, nil
}

// Verify validates a JWT token and returns the claims.
func (j *JWTSession) Verify(token string) (*JWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}

	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(j.secret))
	mac.Write([]byte(signingInput))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(expectedSig)) != 1 {
		return nil, fmt.Errorf("invalid signature")
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	var claims JWTClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, fmt.Errorf("unmarshal claims: %w", err)
	}

	if time.Now().Unix() > claims.ExpiresAt {
		return nil, fmt.Errorf("token expired")
	}

	return &claims, nil
}

// JWTAuthMiddleware returns HTTP middleware that validates JWT tokens.
func (j *JWTSession) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"missing bearer token"}}`))
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		claims, err := j.Verify(token)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"invalid or expired token"}}`))
			return
		}
		r.Header.Set("X-Actor-UserId", claims.UserID)
		r.Header.Set("X-Actor-TenantId", claims.TenantID)
		r.Header.Set("X-Actor-Email", claims.Email)
		r.Header.Set("X-Request-Id", uuid.New().String())
		next.ServeHTTP(w, r)
	})
}

// GenerateRefreshToken creates a random refresh token.
func GenerateRefreshToken() string {
	return "rt_" + uuid.New().String()
}
