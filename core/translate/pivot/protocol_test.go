package pivot

import "testing"

func TestErrorType(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{400, "invalid_request_error"},
		{422, "invalid_request_error"},
		{401, "authentication_error"},
		{402, "permission_error"},
		{403, "permission_error"},
		{404, "not_found_error"},
		{429, "rate_limit_error"},
		{500, "api_error"},
		{503, "api_error"},
		{418, "api_error"},
	} {
		if got := ErrorType(tc.status); got != tc.want {
			t.Errorf("ErrorType(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestRequestError(t *testing.T) {
	var err error = &RequestError{Status: 400, Type: "invalid_request_error", Message: "bad"}
	if got := err.Error(); got != "400 invalid_request_error: bad" {
		t.Errorf("Error() = %q", got)
	}
}
