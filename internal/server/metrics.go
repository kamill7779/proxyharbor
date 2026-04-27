package server

import (
	"fmt"
	"net/http"
	"strings"
)

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	labels := ""
	if s.instanceID != "" {
		labels = fmt.Sprintf("{instance_id=%q}", escapePromLabel(s.instanceID))
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP proxyharbor_instance_up Whether this ProxyHarbor instance is serving metrics.\n")
	_, _ = fmt.Fprintf(w, "# TYPE proxyharbor_instance_up gauge\n")
	_, _ = fmt.Fprintf(w, "proxyharbor_instance_up%s 1\n", labels)
}

func escapePromLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}
