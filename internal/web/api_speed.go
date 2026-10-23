package web

import (
	"net/http"
	"time"

	"github.com/LECYWZA/mytun/internal/mux"
)

// handleSpeed returns the live tunnel throughput plus chart series for the
// 5/15/30 minute windows the right rail draws. One request carries everything
// so the console polls once every few seconds instead of three times.
func (s *Server) handleSpeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET only"})
		return
	}
	m := s.rt.Speed()
	if m == nil {
		writeJSON(w, http.StatusOK, map[string]any{"up": 0, "down": 0})
		return
	}
	up, down := m.Current()
	totalIn, totalOut := mux.TrafficTotals()
	writeJSON(w, http.StatusOK, map[string]any{
		"up":        up,
		"down":      down,
		"totalUp":   totalOut,
		"totalDown": totalIn,
		"ts":        time.Now().UnixMilli(),
		"m5":    m.Series(5*time.Minute, 60),
		"m15":   m.Series(15*time.Minute, 60),
		"m30":   m.Series(30*time.Minute, 60),
	})
}
