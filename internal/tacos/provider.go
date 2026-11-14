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
	maxProviderRequestBody  = 8 << 10
	providerBodyReadTimeout = 10 * time.Second
)

// secretsReadHandler implements the only provider API endpoint. It never lists
// secrets, never mutates them, and never logs request/response bodies, tokens or
// values.
func (s *Server) secretsReadHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeProviderError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	token, ok := s.authorizeProvider(r)
	if !ok {
		writeProviderError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	controller := http.NewResponseController(w)
	deadlineErr := controller.SetReadDeadline(time.Now().Add(providerBodyReadTimeout))
	if deadlineErr != nil && !errors.Is(deadlineErr, http.ErrNotSupported) {
		writeProviderError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if deadlineErr == nil {
		defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxProviderRequestBody)
	decoder := json.NewDecoder(r.Body)
	var req providerapi.ReadSecretRequest
	if err := decoder.Decode(&req); err != nil {
		if isBodyTooLarge(err) {
			writeProviderError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeProviderError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if isBodyTooLarge(err) {
			writeProviderError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeProviderError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !ValidSecretName(req.Name) {
		writeProviderError(w, http.StatusBadRequest, "invalid secret name")
		return
	}

	values, err := s.store.ReadSecretValuesContext(r.Context(), req.Name)
	if err != nil {
		if errors.Is(err, ErrSecretNotFound) {
			writeProviderError(w, http.StatusNotFound, "secret not found")
			return
		}
		writeProviderError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if values == nil {
		values = map[string]string{}
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
		Name:   req.Name,
		Values: values,
	})
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
		if r.URL.Path == providerapi.SecretsReadPath {
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
