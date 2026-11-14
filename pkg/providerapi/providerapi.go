// Package providerapi is the stdlib-only client contract for the MgCl
// ephemeral provider. It deliberately has no dependency on mgcl internals so
// the independently versioned terraform-provider-mgcl repository can import it.
package providerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// EnvEndpoint is the environment variable carrying the provider endpoint.
	EnvEndpoint = "MGCL_PROVIDER_ENDPOINT"
	// EnvToken is the environment variable carrying the provider session token.
	EnvToken = "MGCL_PROVIDER_TOKEN"
	// SecretsReadPath is the only provider API route.
	SecretsReadPath = "/api/mgcl/v1/secrets:read"

	defaultTimeout   = 10 * time.Second
	maxResponseBytes = 2 << 20
)

// ReadSecretRequest is the request body for SecretsReadPath.
type ReadSecretRequest struct {
	Name string `json:"name"`
}

// ReadSecretResponse is the successful response body for SecretsReadPath.
type ReadSecretResponse struct {
	Name   string            `json:"name"`
	Values map[string]string `json:"values"`
}

// Client is a minimal provider API client. Token and secret values are never
// included in returned errors.
type Client struct {
	Endpoint   string
	Token      string
	HTTPClient *http.Client
}

// ReadSecret fetches the current values of one secret. The default HTTP client
// has a finite timeout and never follows redirects, so the token cannot be
// forwarded to another host.
func (c *Client) ReadSecret(ctx context.Context, name string) (*ReadSecretResponse, error) {
	if c == nil {
		return nil, errors.New("provider API client is nil")
	}
	if c.Endpoint == "" {
		return nil, errors.New("provider endpoint is not configured")
	}
	if c.Token == "" {
		return nil, errors.New("provider token is not configured")
	}

	body, err := json.Marshal(ReadSecretRequest{Name: name})
	if err != nil {
		return nil, fmt.Errorf("encode provider request: %w", err)
	}

	url := strings.TrimRight(c.Endpoint, "/") + SecretsReadPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create provider request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-store")

	client := c.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Use only the numeric status. resp.Status includes the server-supplied
		// reason phrase, which an untrusted endpoint could fill with the bearer
		// token or secret values. Never include the response body either.
		return nil, fmt.Errorf("provider API returned status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read provider response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("provider response exceeds size limit")
	}

	var out ReadSecretResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, errors.New("provider response is not valid JSON")
	}
	if out.Values == nil {
		out.Values = map[string]string{}
	}
	return &out, nil
}

func (c *Client) httpClient() *http.Client {
	var client http.Client
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	if client.Timeout == 0 {
		client.Timeout = defaultTimeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}
