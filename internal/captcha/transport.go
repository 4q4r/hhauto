package captcha

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProtocolError is a typed failure returned by a captcha service protocol:
// an ERROR_* code from the 2captcha in.php/res.php family, or errorId>0
// with an errorCode from the anticaptcha createTask/getTaskResult family.
// It never embeds the API key.
type ProtocolError struct {
	ProviderID  string
	Code        string // e.g. "ERROR_KEY_DOES_NOT_EXIST"
	Description string // optional human-readable detail from the service
}

func (e *ProtocolError) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("captcha: %s: service error %s", e.ProviderID, e.Code)
	}
	return fmt.Sprintf("captcha: %s: service error %s: %s", e.ProviderID, e.Code, e.Description)
}

// maxResponseBytes bounds every adapter response body read so a hostile or
// misbehaving service cannot exhaust memory.
const maxResponseBytes = 1 << 20

// httpJSONClient is the shared HTTP plumbing of both protocol adapters.
// It always uses the default transport: certificate verification is
// non-negotiable and no option can turn it off.
type httpJSONClient struct {
	client *http.Client
}

func newHTTPJSONClient() httpJSONClient {
	return httpJSONClient{client: &http.Client{Timeout: 30 * time.Second}}
}

// do performs one protocol request against the service. form values are sent
// as a POST body (keys stay out of URLs); with nil form it issues a GET.
func (c httpJSONClient) do(ctx context.Context, method, endpoint string, form url.Values) ([]byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("captcha: build request: %w", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.roundTrip(ctx, req)
}

// postBody performs one JSON POST with a pre-encoded body.
func (c httpJSONClient) postBody(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("captcha: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.roundTrip(ctx, req)
}

func (c httpJSONClient) roundTrip(ctx context.Context, req *http.Request) ([]byte, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("captcha: %s %s: %w", req.Method, redactedHost(req.URL.String()), err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("captcha: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("captcha: %s: unexpected HTTP status %d", redactedHost(req.URL.String()), resp.StatusCode)
	}
	return data, nil
}

// redactedHost keeps error messages free of query strings (res.php carries
// the API key in its query string by protocol design).
func redactedHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "<endpoint>"
	}
	return u.Host
}

// waitFor sleeps for d or until ctx is done, whichever comes first.
func waitFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
