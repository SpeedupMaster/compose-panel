package docker

import (
	"net/http"
	"testing"
)

func TestClientTransportConfiguresPerHostIdleConns(t *testing.T) {
	c := New("unix:///var/run/docker.sock")
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.http.Transport)
	}
	if tr.MaxIdleConnsPerHost < 8 {
		t.Errorf("MaxIdleConnsPerHost = %d, want >= 8", tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout <= 0 {
		t.Errorf("IdleConnTimeout = %v, want > 0", tr.IdleConnTimeout)
	}
}
