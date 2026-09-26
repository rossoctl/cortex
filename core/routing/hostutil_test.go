package routing

import "testing"

func TestServiceNameFromHost(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"auth-target-service.authbridge.svc.cluster.local:8081", "auth-target-service"},
		{"auth-target-service.authbridge.svc.cluster.local", "auth-target-service"},
		{"auth-target-service:8081", "auth-target-service"},
		{"auth-target-service", "auth-target-service"},
		{"simple", "simple"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ServiceNameFromHost(tt.host); got != tt.want {
			t.Errorf("ServiceNameFromHost(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}
