package exchange

import "fmt"

// ExchangeError represents an OAuth token exchange failure with HTTP status
// and RFC 6749 error/error_description fields for diagnostics.
type ExchangeError struct {
	StatusCode       int
	OAuthError       string // RFC 6749 "error" field
	OAuthDescription string // RFC 6749 "error_description" field
}

func (e *ExchangeError) Error() string {
	if e.OAuthDescription != "" {
		return fmt.Sprintf("token exchange failed (HTTP %d): %s: %s",
			e.StatusCode, e.OAuthError, e.OAuthDescription)
	}
	return fmt.Sprintf("token exchange failed (HTTP %d): %s",
		e.StatusCode, e.OAuthError)
}
