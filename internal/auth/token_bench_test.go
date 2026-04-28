package auth

import (
	"testing"
	"time"
)

func BenchmarkSign(b *testing.B) {
	s := NewSigner("bench-secret")
	p := TokenPayload{
		Key:      "video/bench.mp4",
		Exp:      2000000000,
		IPPrefix: "10.0.0.0/8",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Sign(p)
	}
}

func BenchmarkVerify(b *testing.B) {
	s := NewSigner("bench-secret")
	p := TokenPayload{
		Key: "video/bench.mp4",
		Exp: time.Now().Add(1 * time.Hour).Unix(),
	}
	token, _ := s.Sign(p)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Verify(token)
	}
}
