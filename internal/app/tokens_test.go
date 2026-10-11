package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dortort/wawarden/internal/token"
)

func TestNoTokenReachesTheMetricsOrTheLogs(t *testing.T) {
	adminToken := token.NewAdmin()
	cfg := testConfig(t, adminToken)
	cfg.MetricsEMF = true
	a, logs := open(t, cfg, noClients{})
	a.emfEvery = 20 * time.Millisecond
	stop := run(t, a)
	_, client := token.NewClient()
	stray := token.NewAdmin()
	for _, credential := range []string{client, stray, adminToken} {
		for _, to := range []string{"client", "admin"} {
			for _, path := range []string{"/v1/me", "/v1/chats/" + credential + "/messages?cursor=" + credential, "/admin/v1/clients/" + credential, "/metrics?" + credential} {
				_, _ = do(t, http.MethodGet, addr(t, a, to), path, bearer(credential))
			}
		}
	}
	scraped := mustDo(t, http.MethodGet, addr(t, a, "admin"), "/metrics", bearer(adminToken))
	if scraped.status != http.StatusOK || !strings.Contains(scraped.body, "wawarden_auth_failures_total ") {
		t.Fatalf("/metrics = %d %q", scraped.status, scraped.body)
	}
	waitUntil(t, "an embedded-metric-format line", func() bool { return len(emfLines(logs)) >= 1 })
	if err := stop(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	logs.mu.Lock()
	text := scraped.body + logs.buf.String()
	logs.mu.Unlock()
	for _, secret := range []string{client[len("ww_")+9:], stray[len("wwadm_"):], adminToken[len("wwadm_"):]} {
		if strings.Contains(text, secret) {
			t.Fatalf("a token's secret reached the metrics or the logs")
		}
	}
}
