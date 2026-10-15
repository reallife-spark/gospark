package metrics

import (
	"strings"
	"testing"
)

func TestWriteGatewayStats(t *testing.T) {
	out := render(t, Snapshot{
		Gateway: &GatewayStats{
			MaxInflight: 8,
			MaxQueued:   16,
			Inflight:    3,
			Queued:      2,
			Admitted:    41,
			Rejected:    5,
		},
	})
	for _, want := range []string{
		"gospark_gateway_inflight_requests 3",
		"gospark_gateway_queued_requests 2",
		"gospark_gateway_max_inflight_requests 8",
		"gospark_gateway_max_queued_requests 16",
		"# TYPE gospark_gateway_admitted_total counter",
		"gospark_gateway_admitted_total 41",
		"gospark_gateway_rejected_total 5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestWriteOmitsGatewayWhenUnset(t *testing.T) {
	out := render(t, Snapshot{})
	if strings.Contains(out, "gospark_gateway_") {
		t.Errorf("gateway metrics present without a limiter:\n%s", out)
	}
}
