package health

import (
	"net/http"
	"time"
)

type FailureKind int

const (
	FailureUnknown FailureKind = iota
	FailureConn
	FailureTimeout
	FailureAuth
	FailureProtocol
)

func (k FailureKind) String() string {
	switch k {
	case FailureConn:
		return "conn"
	case FailureTimeout:
		return "timeout"
	case FailureAuth:
		return "auth"
	case FailureProtocol:
		return "protocol"
	default:
		return "unknown"
	}
}

type ScoringPolicy struct {
	SuccessReward        int
	FailurePenalty       map[FailureKind]int
	CircuitOpenThreshold int
	CircuitBaseCooldown  time.Duration
	CircuitMaxCooldown   time.Duration
}

func DefaultScoringPolicy() ScoringPolicy {
	return ScoringPolicy{
		SuccessReward: 5,
		FailurePenalty: map[FailureKind]int{
			FailureUnknown:  5,
			FailureConn:     10,
			FailureTimeout:  15,
			FailureAuth:     30,
			FailureProtocol: 30,
		},
		CircuitOpenThreshold: 3,
		CircuitBaseCooldown:  30 * time.Second,
		CircuitMaxCooldown:   5 * time.Minute,
	}
}

func ScoringPolicyForProfile(profile string) ScoringPolicy {
	policy := DefaultScoringPolicy()
	switch profile {
	case "aggressive":
		policy.FailurePenalty = map[FailureKind]int{
			FailureUnknown:  10,
			FailureConn:     20,
			FailureTimeout:  25,
			FailureAuth:     40,
			FailureProtocol: 40,
		}
		policy.CircuitOpenThreshold = 2
	case "lenient":
		policy.FailurePenalty = map[FailureKind]int{
			FailureUnknown:  2,
			FailureConn:     5,
			FailureTimeout:  8,
			FailureAuth:     20,
			FailureProtocol: 20,
		}
		policy.CircuitOpenThreshold = 5
	}
	return policy
}

// ClassifyProxyHTTPStatus returns whether an HTTP response status is evidence
// of upstream proxy failure rather than the target site's business response.
func ClassifyProxyHTTPStatus(statusCode int, header http.Header) (FailureKind, bool) {
	switch statusCode {
	case http.StatusProxyAuthRequired:
		return FailureAuth, true
	case http.StatusTooManyRequests:
		return FailureProtocol, true
	case http.StatusRequestTimeout:
		if isProxyMarked(header) {
			return FailureTimeout, true
		}
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if isProxyMarked(header) {
			if statusCode == http.StatusGatewayTimeout {
				return FailureTimeout, true
			}
			return FailureConn, true
		}
	}
	return FailureUnknown, false
}

func isProxyMarked(header http.Header) bool {
	return header.Get("Proxy-Authenticate") != "" || header.Get("Proxy-Connection") != "" || header.Get("Via") != ""
}
