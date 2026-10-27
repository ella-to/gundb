package js

import "testing"

func BenchmarkQuote(b *testing.B) {
	for _, tc := range []struct{ name, s string }{
		{"plain", "Writes Go and JS all day, every day of the week"},
		{"escaped", "line one\nline \"two\"\tend"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Quote(tc.s)
			}
		})
	}
}

func BenchmarkCompare(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		Compare("chat/2026-10-27T10:00:00.000Z/alice", "chat/2026-10-27T10:00:00.000Z/bob")
	}
}
