package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("panic recovered", "path", r.URL.Path, "method", r.Method, "panic", recovered, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": domain.ErrorCode(domain.ErrInternal)})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
