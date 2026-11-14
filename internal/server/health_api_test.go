package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeterministicHealthNoDataAndSamples(t *testing.T) {
	s, _ := newTestServer(t)
	for _, populated := range []bool{false, true} {
		if populated {
			fillMinutes(t, s, "n-192-168-50-10", time.Now(), 60, 2, func(ago int) int {
				if ago <= 5 {
					return 0
				}
				return 60
			})
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/health-report", strings.NewReader(`{"range":"24h"}`))
		if err := s.handleHealthReport(w, r); err != nil {
			t.Fatal(err)
		}
		var result map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		k := result["summary"].(map[string]interface{})
		if k["has_data"] != populated {
			t.Fatal(k)
		}
		if !populated && k["availability_pct"] != nil {
			t.Fatal("no data shown as available", k)
		}
		if populated && (k["down_minutes"] != float64(5) || k["fault_episodes"] != float64(1)) {
			t.Fatal(k)
		}
		if result["model"] != "deterministic" || !strings.Contains(result["markdown"].(string), "非激活时段") {
			t.Fatal(result)
		}
	}
}
