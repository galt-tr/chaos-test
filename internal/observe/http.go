package observe

import (
	"context"
	"io"
	"net/http"
	"strings"
)

func httpOK(ctx context.Context, url string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "unreachable"
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	detail := strings.TrimSpace(string(b))
	if len(detail) > 120 {
		detail = detail[:120] + "…"
	}
	return resp.StatusCode/100 == 2, detail
}
