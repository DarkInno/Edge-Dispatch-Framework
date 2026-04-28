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

	_, err := s.Sign(p)
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

func TestMatchesIP(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		ip      string
		want    bool
		wantErr bool
	}{
		{"ipv4 exact", "10.0.0.0/24", "10.0.0.1", true, false},
		{"ipv4 broadcast", "10.0.0.0/24", "10.0.0.255", true, false},
		{"ipv4 outside", "10.0.0.0/24", "10.0.1.1", false, false},
		{"ipv4 gateway", "10.0.0.0/24", "10.0.0.0", true, false},
		{"ipv4 wrong subnet", "10.0.0.0/24", "192.168.0.1", false, false},
		{"ipv6 exact", "2001:db8::/64", "2001:db8::1", true, false},
		{"ipv6 outside", "2001:db8::/64", "2001:db8:1::1", false, false},
		{"empty prefix", "", "10.0.0.1", true, false},
		{"single ip", "10.0.0.1/32", "10.0.0.1", true, false},
		{"single ip mismatch", "10.0.0.1/32", "10.0.0.2", false, false},
		{"invalid ip", "10.0.0.0/24", "not-an-ip", false, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := TokenPayload{Key: "key", Exp: 2000000000, IPPrefix: c.prefix}
			got, err := p.MatchesIP(c.ip)
			if c.wantErr && err == nil {
				t.Error("expected error")
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestComputeIPPrefix(t *testing.T) {
	v4 := ComputeIPPrefix("10.20.30.40")
	if v4 != "10.20.30.0/24" {
		t.Errorf("ipv4: got %q, want %q", v4, "10.20.30.0/24")
	}

	v6 := ComputeIPPrefix("2001:db8:85a3::8a2e:370:7334")
	if v6 != "2001:db8:85a3::/64" {
		t.Errorf("ipv6: got %q, want %q", v6, "2001:db8:85a3::/64")
	}

	empty := ComputeIPPrefix("not-an-ip")
	if empty != "" {
		t.Errorf("invalid: got %q, want empty", empty)
	}
}

func TestVerifyWithIP(t *testing.T) {
	s := NewSigner("secret")
	p := TokenPayload{
		Key:      "key",
		Exp:      time.Now().Add(1 * time.Hour).Unix(),
		IPPrefix: "10.0.0.0/24",
	}
	token, _ := s.Sign(p)

	// Matching IP
	_, err := s.VerifyWithIP(token, "10.0.0.5")
	if err != nil {
		t.Errorf("VerifyWithIP matching IP: %v", err)
	}

	// Non-matching IP
	_, err = s.VerifyWithIP(token, "192.168.1.1")
	if err == nil {
		t.Error("expected error for non-matching IP")
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
