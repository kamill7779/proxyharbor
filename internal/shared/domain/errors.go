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
	ErrBadRequest          = errors.New("bad_request")
	ErrTenantNotFound      = errors.New("tenant_not_found")
	ErrTenantMismatch      = errors.New("tenant_mismatch")
	ErrForbidden           = errors.New("forbidden")
)

type ErrorKind string

const (
	ErrorKindUnknown                 ErrorKind = "unknown"
	ErrorKindSelectorNoCandidates    ErrorKind = "selector_no_candidates"
	ErrorKindSelectorNoEligible      ErrorKind = "selector_no_eligible"
	ErrorKindSelectorRedis           ErrorKind = "selector_redis"
	ErrorKindSelectorEmptyResult     ErrorKind = "selector_empty_result"
	ErrorKindSelectorMalformedResult ErrorKind = "selector_malformed_result"
	ErrorKindSelectorStaleResult     ErrorKind = "selector_stale_result"
	ErrorKindSelectorReadyRebuild    ErrorKind = "selector_ready_empty_after_rebuild"
)

type KindedError struct {
	Public error
	Kind   ErrorKind
	Cause  error
	Reason string
}

func (e *KindedError) Error() string {
	if e == nil || e.Public == nil {
		return ""
	}
	return e.Public.Error()
}

func (e *KindedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Public
}

func (e *KindedError) InternalCause() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func NewKindedError(public error, kind ErrorKind, reason string, cause error) error {
	return &KindedError{Public: public, Kind: kind, Reason: reason, Cause: cause}
}

func ErrorKindOf(err error) ErrorKind {
	var kinded *KindedError
	if errors.As(err, &kinded) && kinded.Kind != "" {
		return kinded.Kind
	}
	return ErrorKindUnknown
}

func ErrorReason(err error) string {
	var kinded *KindedError
	if errors.As(err, &kinded) {
		return kinded.Reason
	}
	return ""
}

func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var kinded *KindedError
	if errors.As(err, &kinded) && kinded.Public != nil {
		return kinded.Public.Error()
	}
	return err.Error()
}
