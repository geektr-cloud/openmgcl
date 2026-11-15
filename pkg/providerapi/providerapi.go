// Package providerapi is the stdlib-only client contract for the MgCl
// provider. It deliberately has no dependency on mgcl internals so the
// independently versioned terraform-provider-mgcl repository can import it.
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
	// SecretsReadPath reads the KVs of one Secret.
	SecretsReadPath = "/api/mgcl/v1/secrets:read"
	// SecretKVsWritePath creates or overwrites one KV.
	SecretKVsWritePath = "/api/mgcl/v1/secret-kvs:write"
	// SecretKVsDeletePath deletes one KV.
	SecretKVsDeletePath = "/api/mgcl/v1/secret-kvs:delete"

	// SecretAccessEphemeral requests every KV of a Secret, including public
	// and ephemeral KVs. It is also the default when ReadSecretRequest.Access
	// is empty.
	SecretAccessEphemeral = "ephemeral"
	// SecretAccessPublic requests only the KVs marked public
	// (is_ephemeral = 0).
	SecretAccessPublic = "public"

	defaultTimeout   = 10 * time.Second
	maxResponseBytes = 2 << 20
)

// ReadSecretRequest is the request body for SecretsReadPath.
type ReadSecretRequest struct {
	Name   string `json:"name"`
	Access string `json:"access,omitempty"`
}

// ReadSecretResponse is the successful response body for SecretsReadPath.
// EphemeralMarks maps each returned KV key to its is_ephemeral mark. A true
// value means the KV is ephemeral; false means it is public.
type ReadSecretResponse struct {
	Name           string            `json:"name"`
	Values         map[string]string `json:"values"`
	EphemeralMarks map[string]bool   `json:"ephemeral_marks"`
}

// WriteSecretKVRequest is the request body for SecretKVsWritePath.
// IsEphemeral is a pointer so an omitted field is distinguishable from an
// explicit false. The server treats a missing field as true (ephemeral), never
// as public.
type WriteSecretKVRequest struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	IsEphemeral *bool  `json:"is_ephemeral,omitempty"`
}

// WriteSecretKVResponse is the successful response body for
// SecretKVsWritePath. It never contains the value.
type WriteSecretKVResponse struct {
	Name        string `json:"name"`
	Key         string `json:"key"`
	IsEphemeral bool   `json:"is_ephemeral"`
}

// DeleteSecretKVRequest is the request body for SecretKVsDeletePath.
type DeleteSecretKVRequest struct {
	Key string `json:"key"`
}

// DeleteSecretKVResponse is the successful response body for
// SecretKVsDeletePath.
type DeleteSecretKVResponse struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Client is a minimal provider API client. Token and secret values are never
// included in returned errors.
type Client struct {
	Endpoint   string
	Token      string
	HTTPClient *http.Client
}

// ReadSecret fetches every KV of one Secret. It sends access "ephemeral",
// which returns public and ephemeral KVs together with their marks. The
// default HTTP client has a finite timeout and never follows redirects, so the
// token cannot be forwarded to another host.
func (c *Client) ReadSecret(ctx context.Context, name string) (*ReadSecretResponse, error) {
	return c.readSecret(ctx, name, SecretAccessEphemeral)
}

// ReadPublicSecret fetches only the public KVs of one Secret. It sends access
// "public"; a Secret with no public KVs returns empty maps.
func (c *Client) ReadPublicSecret(ctx context.Context, name string) (*ReadSecretResponse, error) {
	return c.readSecret(ctx, name, SecretAccessPublic)
}

// WriteSecretKV creates or overwrites one KV. A missing Secret is created
// atomically together with its DEK. isEphemeral sets the KV's access mark.
func (c *Client) WriteSecretKV(ctx context.Context, ref, value string, isEphemeral bool) (*WriteSecretKVResponse, error) {
	var out WriteSecretKVResponse
	err := c.post(ctx, SecretKVsWritePath, WriteSecretKVRequest{
		Key:         ref,
		Value:       value,
		IsEphemeral: &isEphemeral,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSecretKV deletes one KV. Deleting a missing Secret or KV succeeds and
// never creates a Secret.
func (c *Client) DeleteSecretKV(ctx context.Context, ref string) (*DeleteSecretKVResponse, error) {
	var out DeleteSecretKVResponse
	err := c.post(ctx, SecretKVsDeletePath, DeleteSecretKVRequest{Key: ref}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) readSecret(ctx context.Context, name, access string) (*ReadSecretResponse, error) {
	var out ReadSecretResponse
	err := c.post(ctx, SecretsReadPath, ReadSecretRequest{Name: name, Access: access}, &out)
	if err != nil {
		return nil, err
	}
	if out.Values == nil {
		out.Values = map[string]string{}
	}
	if out.EphemeralMarks == nil {
		out.EphemeralMarks = map[string]bool{}
	}
	return &out, nil
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	if c == nil {
		return errors.New("provider API client is nil")
	}
	if c.Endpoint == "" {
		return errors.New("provider endpoint is not configured")
	}
	if c.Token == "" {
		return errors.New("provider token is not configured")
	}

	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode provider request: %w", err)
	}

	url := strings.TrimRight(c.Endpoint, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create provider request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-store")

	client := c.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("provider request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Use only the numeric status. resp.Status includes the server-supplied
		// reason phrase, which an untrusted endpoint could fill with the bearer
		// token or secret values. Never include the response body either.
		return fmt.Errorf("provider API returned status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read provider response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return errors.New("provider response exceeds size limit")
	}

	if err := json.Unmarshal(data, out); err != nil {
		return errors.New("provider response is not valid JSON")
	}
	return nil
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
