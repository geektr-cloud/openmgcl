package tacos

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
)

const (
	maxProviderReadRequestBody  = 8 << 10
	maxProviderWriteRequestBody = 512 << 10
	maxProviderBatchRequestBody = 2 << 20
	providerBodyReadTimeout     = 10 * time.Second
)

// secretsReadHandler reads KVs. access "ephemeral" (or empty) returns every KV
// with its mark; access "public" returns only public KVs. It never lists
// Secrets, never logs request/response bodies, tokens or values.
func (s *Server) secretsReadHandler(w http.ResponseWriter, r *http.Request) {
	token, cleanup, ok := s.beginProviderRequest(w, r)
	if !ok {
		return
	}
	defer cleanup()
	var req providerapi.ReadSecretRequest
	if !decodeProviderJSON(w, r, maxProviderReadRequestBody, &req) {
		return
	}
	if !ValidSecretName(req.Name) {
		writeProviderError(w, http.StatusBadRequest, "invalid secret name")
		return
	}
	allKeys, ok := providerAccess(req.Access)
	if !ok {
		writeProviderError(w, http.StatusBadRequest, "invalid access")
		return
	}

	values, marks, err := s.store.ReadSecretValuesContextAccess(r.Context(), req.Name, allKeys)
	if err != nil {
		switch {
		case errors.Is(err, ErrSecretNotFound):
			writeProviderError(w, http.StatusNotFound, "secret not found")
		case errors.Is(err, ErrSecretRecipientMismatch):
			writeProviderError(w, http.StatusForbidden, "secret key access denied")
		default:
			writeProviderError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if values == nil {
		values = map[string]string{}
	}
	if marks == nil {
		marks = map[string]bool{}
	}

	// Re-check the same token after successful decryption and immediately
	// before writing plaintext. A session revoked while the handler was
	// reading must not receive values.
	if s.afterProviderDecrypt != nil {
		s.afterProviderDecrypt()
	}
	if !s.providers.Authorize(token) {
		writeProviderError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(providerapi.ReadSecretResponse{
		Name:           req.Name,
		Values:         values,
		EphemeralMarks: marks,
	})
}

// secretKVsWriteHandler creates or overwrites one KV. A missing Secret is
// created atomically with its DEK; existing KVs update value and mark.
func (s *Server) secretKVsWriteHandler(w http.ResponseWriter, r *http.Request) {
	_, cleanup, ok := s.beginProviderRequest(w, r)
	if !ok {
		return
	}
	defer cleanup()
	var req providerapi.WriteSecretKVRequest
	if !decodeProviderJSON(w, r, maxProviderWriteRequestBody, &req) {
		return
	}
	if _, _, err := ParseSecretReference(req.Key); err != nil {
		writeProviderError(w, http.StatusBadRequest, "invalid secret reference")
		return
	}
	// A missing is_ephemeral field means ephemeral, never public.
	isEphemeral := true
	if req.IsEphemeral != nil {
		isEphemeral = *req.IsEphemeral
	}
	name, key, err := s.store.WriteSecretKVContext(r.Context(), req.Key, []byte(req.Value), isEphemeral)
	if err != nil {
		switch {
		case errors.Is(err, ErrSecretInvalidValue):
			writeProviderError(w, http.StatusBadRequest, "invalid secret value")
		case errors.Is(err, ErrSecretLimitExceeded):
			writeProviderError(w, http.StatusBadRequest, "secret limit exceeded")
		case errors.Is(err, ErrSecretRecipientMismatch):
			writeProviderError(w, http.StatusForbidden, "secret key access denied")
		default:
			writeProviderError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(providerapi.WriteSecretKVResponse{
		Name:        name,
		Key:         key,
		IsEphemeral: isEphemeral,
	})
}

// secretKVsDeleteHandler deletes one KV. Missing Secrets and KVs are
// idempotent successes and never create a Secret.
func (s *Server) secretKVsDeleteHandler(w http.ResponseWriter, r *http.Request) {
	_, cleanup, ok := s.beginProviderRequest(w, r)
	if !ok {
		return
	}
	defer cleanup()
	var req providerapi.DeleteSecretKVRequest
	if !decodeProviderJSON(w, r, maxProviderReadRequestBody, &req) {
		return
	}
	if _, _, err := ParseSecretReference(req.Key); err != nil {
		writeProviderError(w, http.StatusBadRequest, "invalid secret reference")
		return
	}
	name, key, err := s.store.DeleteSecretKVContext(r.Context(), req.Key)
	if err != nil {
		writeProviderError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(providerapi.DeleteSecretKVResponse{Name: name, Key: key})
}

// secretKVsWriteBatchHandler atomically upserts and idempotently deletes KVs.
// It never returns values. A missing Secret is created with its DEK when
// ensure is true or when at least one KV is upserted; a pure delete is a
// no-op success for a missing Secret.
func (s *Server) secretKVsWriteBatchHandler(w http.ResponseWriter, r *http.Request) {
	_, cleanup, ok := s.beginProviderRequest(w, r)
	if !ok {
		return
	}
	defer cleanup()
	var req providerapi.WriteSecretKVsRequest
	if !decodeProviderJSON(w, r, maxProviderBatchRequestBody, &req) {
		return
	}
	if !ValidSecretName(req.Name) {
		writeProviderError(w, http.StatusBadRequest, "invalid secret name")
		return
	}

	kvs := make([]SecretKVWrite, 0, len(req.KVs))
	for _, item := range req.KVs {
		kvs = append(kvs, SecretKVWrite{
			Key:         item.Key,
			Value:       []byte(item.Value),
			IsEphemeral: item.IsEphemeral,
		})
	}
	results, err := s.store.WriteSecretKVsContext(r.Context(), req.Name, kvs, req.DeleteKeys, req.Ensure)
	if err != nil {
		switch {
		case errors.Is(err, ErrSecretInvalidValue):
			writeProviderError(w, http.StatusBadRequest, "invalid secret value")
		case errors.Is(err, ErrSecretInvalidBatch):
			writeProviderError(w, http.StatusBadRequest, "invalid request")
		case errors.Is(err, ErrSecretLimitExceeded):
			writeProviderError(w, http.StatusBadRequest, "secret limit exceeded")
		case errors.Is(err, ErrSecretRecipientMismatch):
			writeProviderError(w, http.StatusForbidden, "secret key access denied")
		default:
			writeProviderError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	out := make([]providerapi.SecretKVWriteMetadata, 0, len(results))
	for _, item := range results {
		out = append(out, providerapi.SecretKVWriteMetadata{Key: item.Key, IsEphemeral: item.IsEphemeral})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(providerapi.WriteSecretKVsResponse{Name: req.Name, KVs: out})
}

// beginProviderRequest applies the shared provider API guards: no-store
// headers, POST-only, provider-session authorization and a bounded read
// deadline. The returned cleanup resets the deadline.
func (s *Server) beginProviderRequest(w http.ResponseWriter, r *http.Request) (string, func(), bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeProviderError(w, http.StatusMethodNotAllowed, "method not allowed")
		return "", func() {}, false
	}
	token, ok := s.authorizeProvider(r)
	if !ok {
		writeProviderError(w, http.StatusUnauthorized, "unauthorized")
		return "", func() {}, false
	}

	controller := http.NewResponseController(w)
	deadlineErr := controller.SetReadDeadline(time.Now().Add(providerBodyReadTimeout))
	if deadlineErr != nil && !errors.Is(deadlineErr, http.ErrNotSupported) {
		writeProviderError(w, http.StatusInternalServerError, "internal error")
		return "", func() {}, false
	}
	if deadlineErr != nil {
		return token, func() {}, true
	}
	return token, func() { _ = controller.SetReadDeadline(time.Time{}) }, true
}

func decodeProviderJSON(w http.ResponseWriter, r *http.Request, maxBody int64, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		if isBodyTooLarge(err) {
			writeProviderError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeProviderError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if isBodyTooLarge(err) {
			writeProviderError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeProviderError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// providerAccess maps the wire access value to a read mode. Empty access
// preserves the v0.1.0 behavior and means ephemeral (all KVs).
func providerAccess(access string) (allKeys bool, ok bool) {
	switch access {
	case "", providerapi.SecretAccessEphemeral:
		return true, true
	case providerapi.SecretAccessPublic:
		return false, true
	default:
		return false, false
	}
}

func (s *Server) authorizeProvider(r *http.Request) (string, bool) {
	if s.providers == nil {
		return "", false
	}
	token, ok := bearerToken(r)
	if !ok {
		return "", false
	}
	return token, s.providers.Authorize(token)
}

// rejectProviderTokenMiddleware keeps provider session tokens out of TACOS
// routes even when TACOS authentication is disabled.
func (s *Server) rejectProviderTokenMiddleware(next http.Handler) http.Handler {
	if s.providers == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProviderPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if token, ok := bearerToken(r); ok && s.providers.Authorize(token) {
			writeError(w, http.StatusUnauthorized, "Unauthorized", "provider token is not a user token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isProviderPath(path string) bool {
	switch path {
	case providerapi.SecretsReadPath, providerapi.SecretKVsWritePath, providerapi.SecretKVsDeletePath, providerapi.SecretKVsWriteBatchPath:
		return true
	default:
		return false
	}
}

func bearerToken(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func writeProviderError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
