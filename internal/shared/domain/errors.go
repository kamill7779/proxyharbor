package domain

import "errors"

var (
	ErrPolicyDenied        = errors.New("policy_denied")
	ErrSubjectNotEligible  = errors.New("subject_not_eligible")
	ErrIdempotencyConflict = errors.New("idempotency_conflict")
	ErrNoHealthyProxy      = errors.New("no_healthy_proxy")
	ErrLeaseRevoked        = errors.New("lease_revoked")
	ErrUnsafeDestination   = errors.New("unsafe_destination")
	ErrCatalogStale        = errors.New("catalog_stale")
	ErrAuthFailed          = errors.New("auth_failed")
	ErrTenantDisabled      = errors.New("tenant_disabled")
	ErrNotFound            = errors.New("not_found")
	ErrUnsupported         = errors.New("unsupported")
)

func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
