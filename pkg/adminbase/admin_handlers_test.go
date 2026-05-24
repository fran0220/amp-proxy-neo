package adminbase

import (
	"net/http/httptest"
	"testing"
)

func TestParseTimeWindow(t *testing.T) {
	cases := []string{"24h", "7d", "14d", "30d", "90d"}
	for _, c := range cases {
		if _, ok := parseTimeWindow(c); !ok {
			t.Fatalf("expected window %s to be supported", c)
		}
	}
	if _, ok := parseTimeWindow("invalid"); ok {
		t.Fatal("expected invalid window to be rejected")
	}
}

func TestParseStatsFilter(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/stats?provider=openai&route=apikey&model=gpt-5.4&window=7d", nil)
	filter := parseStatsFilter(req)

	if filter.Provider != "openai" {
		t.Fatalf("expected provider=openai, got %q", filter.Provider)
	}
	if filter.Route != "apikey" {
		t.Fatalf("expected route=apikey, got %q", filter.Route)
	}
	if filter.Model != "gpt-5.4" {
		t.Fatalf("expected model=gpt-5.4, got %q", filter.Model)
	}
	if filter.Since.IsZero() {
		t.Fatal("expected since to be set by window filter")
	}
}
