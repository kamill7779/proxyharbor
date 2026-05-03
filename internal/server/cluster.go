package server

import (
	"net/http"
	"time"

	"github.com/kamill7779/proxyharbor/internal/cluster"
)

func (s *Server) adminCluster(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	body := map[string]any{
		"instance_id": s.instanceID,
		"role":        string(s.role),
	}
	for key, value := range s.clusterSummary {
		body[key] = value
	}
	if s.clusterStore != nil {
		lock, ok, err := s.clusterStore.GetClusterLock(r.Context(), cluster.GlobalMaintenanceLock)
		if err != nil {
			body["leader_error_kind"] = classifyDependencyError("cluster", err)
		} else if ok {
			body["leader"] = map[string]any{
				"lock":              lock.Name,
				"owner_instance_id": lock.OwnerInstanceID,
				"lease_until":       lock.LeaseUntil,
				"active":            lock.LeaseUntil.After(time.Now().UTC()),
			}
		} else {
			body["leader"] = nil
		}
	}
	writeJSON(w, http.StatusOK, body)
}
