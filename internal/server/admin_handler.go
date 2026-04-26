package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

// AdminStore abstracts tenant and key management operations.
type AdminStore interface {
	GetTenant(ctx context.Context, id string) (domain.Tenant, error)
	ListTenants(ctx context.Context) ([]domain.Tenant, error)
	CreateTenant(ctx context.Context, tenant domain.Tenant) error
	UpdateTenant(ctx context.Context, id string, displayName *string, status *string) error
	SoftDeleteTenant(ctx context.Context, id string) error
	ListTenantKeys(ctx context.Context, tenantID string) ([]auth.TenantKey, error)
	CreateTenantKey(ctx context.Context, key auth.TenantKey) error
	RevokeTenantKey(ctx context.Context, tenantID, keyID string) error
	IncrementTenantKeysVersion(ctx context.Context) error
	AppendAuditEvents(ctx context.Context, events []domain.AuditEvent) error
}

type adminHandler struct {
	store  AdminStore
	pepper string
}

func newAdminHandler(store AdminStore, pepper string) *adminHandler {
	return &adminHandler{store: store, pepper: pepper}
}

func (h *adminHandler) register(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/admin/tenants", wrap(h.tenants))
	mux.HandleFunc("/admin/tenants/", wrap(h.tenantByID))
}

func (h *adminHandler) tenants(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		}
		if !decode(w, r, &req) {
			return
		}
		if req.ID == "" || req.DisplayName == "" {
			respond(w, nil, domain.ErrBadRequest, http.StatusOK)
			return
		}
		now := time.Now().UTC()
		tenant := domain.Tenant{
			ID:        req.ID,
			Name:      req.DisplayName,
			Enabled:   true,
			CreatedAt: now,
		}
		if err := h.store.CreateTenant(r.Context(), tenant); err != nil {
			respond(w, nil, err, http.StatusOK)
			return
		}
		writeJSON(w, http.StatusCreated, tenant)
	case http.MethodGet:
		list, err := h.store.ListTenants(r.Context())
		if err != nil {
			respond(w, nil, err, http.StatusOK)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tenants": list})
	default:
		methodNotAllowed(w)
	}
}

func (h *adminHandler) tenantByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/tenants/")
	parts := strings.SplitN(path, "/", 3)
	id := parts[0]
	if id == "" {
		respond(w, nil, domain.ErrNotFound, http.StatusOK)
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPatch:
			var req struct {
				DisplayName *string `json:"display_name,omitempty"`
				Status      *string `json:"status,omitempty"`
			}
			if !decode(w, r, &req) {
				return
			}
			if req.DisplayName == nil && req.Status == nil {
				respond(w, nil, domain.ErrBadRequest, http.StatusOK)
				return
			}
			if err := h.store.UpdateTenant(r.Context(), id, req.DisplayName, req.Status); err != nil {
				respond(w, nil, err, http.StatusOK)
				return
			}
			if req.Status != nil && (*req.Status == "disabled" || *req.Status == "deleted") {
				if err := h.revokeTenantKeys(r.Context(), id); err != nil {
					respond(w, nil, err, http.StatusOK)
					return
				}
			}
			tenant, err := h.store.GetTenant(r.Context(), id)
			if err != nil {
				respond(w, nil, err, http.StatusOK)
				return
			}
			writeJSON(w, http.StatusOK, tenant)
		case http.MethodDelete:
			if err := h.store.SoftDeleteTenant(r.Context(), id); err != nil {
				respond(w, nil, err, http.StatusOK)
				return
			}
			if err := h.revokeTenantKeys(r.Context(), id); err != nil {
				respond(w, nil, err, http.StatusOK)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		default:
			methodNotAllowed(w)
		}
		return
	}

	// Handle /admin/tenants/{id}/keys (no trailing slash) as well.
	if len(parts) == 2 && parts[1] == "keys" {
		parts = append(parts, "")
	}
	if len(parts) >= 3 && parts[1] == "keys" {
		keyID := parts[2]
		if keyID == "" && r.Method == http.MethodPost {
			// POST /admin/tenants/{id}/keys
			var req struct {
				Label      string `json:"label"`
				Purpose    string `json:"purpose"`
				TTLSeconds int    `json:"ttl_seconds"`
			}
			if !decode(w, r, &req) {
				return
			}
			if req.Label == "" {
				req.Label = "default"
			}
			if req.Purpose == "" {
				req.Purpose = "general"
			}
			plaintext := generateKey()
			keyHash := hashWithPepper(h.pepper, plaintext)
			keyFP := fingerprint(plaintext)
			now := time.Now().UTC()
			tk := auth.TenantKey{
				ID:        "k_" + randomKeyID(),
				TenantID:  id,
				KeyHash:   keyHash,
				KeyFP:     keyFP,
				Label:     req.Label,
				Purpose:   req.Purpose,
				CreatedBy: "admin",
				CreatedAt: now,
			}
			if req.TTLSeconds > 0 {
				exp := now.Add(time.Duration(req.TTLSeconds) * time.Second)
				tk.ExpiresAt = &exp
			}
			if err := h.store.CreateTenantKey(r.Context(), tk); err != nil {
				respond(w, nil, err, http.StatusOK)
				return
			}
			_ = h.store.AppendAuditEvents(r.Context(), []domain.AuditEvent{{
				EventID:     "audit-" + tk.ID,
				TenantID:    id,
				PrincipalID: "admin",
				Action:      "tenant_key.issued",
				Resource:    tk.ID,
				OccurredAt:  now,
				Metadata:    map[string]string{"key_fp": keyFP, "purpose": req.Purpose},
			}})
			writeJSON(w, http.StatusCreated, map[string]any{
				"key_id":     tk.ID,
				"tenant_id":  id,
				"key":        plaintext,
				"key_fp":     keyFP,
				"expires_at": tk.ExpiresAt,
				"created_at": tk.CreatedAt.Format(time.RFC3339),
			})
			return
		}
		if keyID == "" && r.Method == http.MethodGet {
			// GET /admin/tenants/{id}/keys
			keys, err := h.store.ListTenantKeys(r.Context(), id)
			if err != nil {
				respond(w, nil, err, http.StatusOK)
				return
			}
			out := make([]map[string]any, 0, len(keys))
			for _, k := range keys {
				out = append(out, map[string]any{
					"key_id":     k.ID,
					"tenant_id":  k.TenantID,
					"key_fp":     k.KeyFP,
					"label":      k.Label,
					"purpose":    k.Purpose,
					"expires_at": k.ExpiresAt,
					"revoked_at": k.RevokedAt,
					"created_at": k.CreatedAt.Format(time.RFC3339),
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"keys": out})
			return
		}
		if keyID != "" && r.Method == http.MethodDelete {
			// DELETE /admin/tenants/{id}/keys/{kid}
			if err := h.store.RevokeTenantKey(r.Context(), id, keyID); err != nil {
				respond(w, nil, err, http.StatusOK)
				return
			}
			_ = h.store.AppendAuditEvents(r.Context(), []domain.AuditEvent{{
				EventID:     "audit-revoke-" + keyID,
				TenantID:    id,
				PrincipalID: "admin",
				Action:      "tenant_key.revoked",
				Resource:    keyID,
				OccurredAt:  time.Now().UTC(),
			}})
			writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
			return
		}
		methodNotAllowed(w)
		return
	}

	respond(w, nil, domain.ErrNotFound, http.StatusOK)
}

func (h *adminHandler) revokeTenantKeys(ctx context.Context, tenantID string) error {
	keys, err := h.store.ListTenantKeys(ctx, tenantID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key.RevokedAt != nil {
			continue
		}
		if err := h.store.RevokeTenantKey(ctx, tenantID, key.ID); err != nil {
			return err
		}
	}
	return nil
}

func generateKey() string {
	env := os.Getenv("PROXYHARBOR_ENV")
	if env == "" {
		env = "live"
	}
	return "phk_" + env + "_" + randomHex(32)
}

func hashWithPepper(pepper, key string) string {
	h := sha256.Sum256([]byte(pepper + key))
	return hex.EncodeToString(h[:])
}

func fingerprint(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:4])
}

func randomKeyID() string {
	return randomHex(8)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
