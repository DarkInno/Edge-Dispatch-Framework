package auth

import (
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	s := NewSigner("test-secret")
	p := TokenPayload{
		Key:      "video/test.mp4",
		Exp:      time.Now().Add(1 * time.Hour).Unix(),
		IPPrefix: "10.0.0.0/8",
	}

	token, err := s.Sign(p)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}
	if token == "" {
		t.Fatal("Sign() returned empty token")
	}

	got, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if got.Key != p.Key {
		t.Errorf("Key: got %q, want %q", got.Key, p.Key)
	}
	if got.Exp != p.Exp {
		t.Errorf("Exp: got %d, want %d", got.Exp, p.Exp)
	}
	if got.IPPrefix != p.IPPrefix {
		t.Errorf("IPPrefix: got %q, want %q", got.IPPrefix, p.IPPrefix)
	}
}

func TestSignAutoExpiry(t *testing.T) {
	s := NewSigner("secret")
	p := TokenPayload{Key: "key"}

	token, err := s.Sign(p)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	got, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	// Should be about 5 minutes from now
	now := time.Now().Unix()
	if got.Exp < now || got.Exp > now+310 {
		t.Errorf("Exp %d not within expected range (now=%d)", got.Exp, now)
	}
}

func TestVerifyExpiredToken(t *testing.T) {
	s := NewSigner("secret")
	p := TokenPayload{
		Key: "key",
		Exp: time.Now().Add(-1 * time.Hour).Unix(),
	}

	token, err := s.Sign(p)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	_, err = s.Verify(token)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestVerifyWrongSecret(t *testing.T) {
	s1 := NewSigner("secret-a")
	s2 := NewSigner("secret-b")
	p := TokenPayload{
		Key: "key",
		Exp: time.Now().Add(1 * time.Hour).Unix(),
	}

	token, err := s1.Sign(p)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	_, err = s2.Verify(token)
	if err == nil {
		t.Fatal("expected error when verifying with wrong secret")
	}
}

func TestVerifyMalformedToken(t *testing.T) {
	s := NewSigner("secret")

	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"single part", "abc"},
		{"three parts", "a.b.c"},
		{"invalid base64", "!!!.!!!"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Verify(c.token)
			if err == nil {
				t.Fatal("expected error for malformed token")
			}
		})
	}
}

func TestVerifyTamperedToken(t *testing.T) {
	s := NewSigner("secret")
	p := TokenPayload{
		Key: "video/test.mp4",
		Exp: time.Now().Add(1 * time.Hour).Unix(),
	}

	token, err := s.Sign(p)
	if err != nil {
		t.Fatalf("Sign() error: %v", err)
	}

	// Tamper with the payload part
	tampered := "Z29vZA.YmFk" // different payload
	_, err = s.Verify(tampered)
	if err == nil {
		t.Fatal("expected error for tampered token")
	}
}

func TestSignDeterministic(t *testing.T) {
	s := NewSigner("secret")
	p := TokenPayload{
		Key: "key",
		Exp: 2000000000,
	}

	t1, err := s.Sign(p)
	if err != nil {
		t.Fatalf("first Sign() error: %v", err)
	}
	t2, err := s.Sign(p)
	if err != nil {
		t.Fatalf("second Sign() error: %v", err)
	}
	if t1 != t2 {
		t.Error("Sign() should be deterministic for same inputs")
	}
}
