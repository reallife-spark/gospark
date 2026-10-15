package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reallife-spark/goSpark/internal/gateway"
)

func waitInflight(t *testing.T, l *gateway.Limiter, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for l.Stats().Inflight != want {
		if time.Now().After(deadline) {
			t.Fatalf("inflight = %d, want %d", l.Stats().Inflight, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestProxyAdmissionRefusesWhenFull proves the inference route is bounded while
// model discovery stays answerable.
func TestProxyAdmissionRefusesWhenFull(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	g := newGateway(upstream.URL)
	g.Limiter = gateway.NewLimiter(1, 0, 0)
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	first := make(chan int, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
		if err != nil {
			first <- -1
			return
		}
		resp.Body.Close()
		first <- resp.StatusCode
	}()
	waitInflight(t, g.Limiter, 1)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header on a refusal")
	}

	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
}
