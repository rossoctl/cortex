package tokenbroker

import "fmt"

// BrokerError represents a Token Broker HTTP failure with RFC 6749-compliant error details.
type BrokerError struct {
	StatusCode       int
	OAuthError       string // RFC 6749 "error" field
	OAuthDescription string // RFC 6749 "error_description" field (mapped from "message")
}

func (e *BrokerError) Error() string {
	if e.OAuthDescription != "" {
		return fmt.Sprintf("token broker failed (HTTP %d): %s: %s",
			e.StatusCode, e.OAuthError, e.OAuthDescription)
	}
	return fmt.Sprintf("token broker failed (HTTP %d): %s",
		e.StatusCode, e.OAuthError)
}
