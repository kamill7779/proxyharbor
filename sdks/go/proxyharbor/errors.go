package proxyharbor

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError is returned for any non-2xx response from ProxyHarbor.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("proxyharbor: %d %s: %s", e.StatusCode, e.Code, e.Message)
	case e.Code != "":
		return fmt.Sprintf("proxyharbor: %d %s", e.StatusCode, e.Code)
	case e.Message != "":
		return fmt.Sprintf("proxyharbor: %d %s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("proxyharbor: http %d", e.StatusCode)
	}
}

// Sentinel errors returned by the SDK.
var (
	ErrNoBaseURL    = errors.New("proxyharbor: PROXYHARBOR_BASE_URL is not configured")
	ErrNoTenantKey  = errors.New("proxyharbor: PROXYHARBOR_TENANT_KEY is not configured")
	ErrNoAdminKey   = errors.New("proxyharbor: PROXYHARBOR_ADMIN_KEY is not configured")
	ErrLeaseExpired = errors.New("proxyharbor: lease expired and AutoReacquire is disabled")
)

// IsUnauthorized reports whether the error is a 401/403 from the API.
func IsUnauthorized(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden
	}
	return false
}

// IsNotFound reports whether the error is a 404 from the API.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}
	return false
}

// IsRetryable reports whether the SDK considers the error worth retrying.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusRequestTimeout,
			http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	// non-API error: assume transport-level, retryable.
	return true
}

// IsLeaseExpired reports whether the error is the ErrLeaseExpired sentinel.
func IsLeaseExpired(err error) bool { return errors.Is(err, ErrLeaseExpired) }
