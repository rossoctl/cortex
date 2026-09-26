package routing

// ServiceNameFromHost extracts the service name (first DNS label) from a Kubernetes host.
// "auth-target-service.team1.svc.cluster.local:8081" becomes "auth-target-service".
func ServiceNameFromHost(host string) string {
	// Strip port
	for i, c := range host {
		if c == ':' {
			host = host[:i]
			break
		}
	}
	// Take first DNS label
	for i, c := range host {
		if c == '.' {
			return host[:i]
		}
	}
	return host
}
