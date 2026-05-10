package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// =============================================================================
// Benchmark Tests
// =============================================================================

func BenchmarkAcquireToken_Success(b *testing.B) {
	helper := NewTestHelper(b)
	srv := helper.NewSuccessBroker("bench-token")
	defer srv.Close()

	client := NewClient()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := client.AcquireToken(ctx, srv.URL, "user-token", "https://api.github.com", "", "")
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

func BenchmarkAcquireToken_Error(b *testing.B) {
	helper := NewTestHelper(b)
	srv := helper.NewErrorBroker(http.StatusUnauthorized, "unauthorized", "test error")
	defer srv.Close()

	client := NewClient()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = client.AcquireToken(ctx, srv.URL, "user-token", "https://api.github.com", "", "")
	}
}

func BenchmarkAcquireToken_LargeToken(b *testing.B) {
	largeToken := strings.Repeat("x", 8192) // 8KB token
	helper := NewTestHelper(b)
	srv := helper.NewSuccessBroker(largeToken)
	defer srv.Close()

	client := NewClient()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := client.AcquireToken(ctx, srv.URL, "user-token", "https://api.github.com", "", "")
		if err != nil {
			b.Fatalf("unexpected error: %v", err)
		}
	}
}

func BenchmarkAcquireToken_Parallel(b *testing.B) {
	helper := NewTestHelper(b)
	srv := helper.NewSuccessBroker("bench-token")
	defer srv.Close()

	client := NewClient()
	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = client.AcquireToken(ctx, srv.URL, "user-token", "https://api.example.com", "", "")
		}
	})
}

func BenchmarkAcquireToken_Allocations(b *testing.B) {
	helper := NewTestHelper(b)
	srv := helper.NewSuccessBroker("alloc-token")
	defer srv.Close()

	client := NewClient()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = client.AcquireToken(ctx, srv.URL, "user-token", "https://api.example.com", "", "")
	}
}

func BenchmarkNewClient(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = NewClient()
	}
}

func BenchmarkBrokerError_Error(b *testing.B) {
	err := &BrokerError{
		StatusCode:       401,
		OAuthError:       "unauthorized",
		OAuthDescription: "test error message",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = err.Error()
	}
}
