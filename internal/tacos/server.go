package tacos

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server holds shared state for the TACOS API handlers.
type Server struct {
	store     *Store
	serverURL string // base URL without trailing slash, e.g. "http://127.0.0.1:1217"
	token     string // user Bearer token; empty means every authenticated route rejects
	providers *ProviderSessions

	// afterProviderDecrypt is a deterministic test hook used to revoke a
	// session between decryption and the final authorization check.
	afterProviderDecrypt func()
}

// NewRouter returns the complete TACOS state.v2 HTTP handler. The user token is
// always required for authenticated routes; an empty token does not disable
// authentication. An optional provider session set enables the ephemeral Secret
// read API; without it that endpoint always returns 401.
func NewRouter(store *Store, serverURL, token string, providers ...*ProviderSessions) http.Handler {
	s := &Server{
		store:     store,
		serverURL: strings.TrimRight(serverURL, "/"),
		token:     token,
	}
	if len(providers) > 0 {
		s.providers = providers[0]
	}
	return buildRouter(s)
}

// buildRouter constructs the chi router for a given Server. Exported for tests
// that need to update serverURL after the listener port is known.
func buildRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(apiVersionMiddleware)
	// Provider session tokens are never valid TACOS credentials, including on
	// auth-exempt upload/download routes and unauthenticated discovery/ping.
	r.Use(s.rejectProviderTokenMiddleware)

	// Discovery document
	r.Get("/.well-known/terraform.json", s.discoveryHandler)

	// Provider Secret API. It is deliberately outside /api/v2 so provider
	// tokens can never be confused with TACOS user tokens.
	r.HandleFunc(providerapi.SecretsReadPath, s.secretsReadHandler)
	r.HandleFunc(providerapi.SecretKVsWritePath, s.secretKVsWriteHandler)
	r.HandleFunc(providerapi.SecretKVsDeletePath, s.secretKVsDeleteHandler)

	// All API routes under /api/v2
	r.Route("/api/v2", func(r chi.Router) {
		r.Get("/ping", s.pingHandler)

		// Auth-required routes. The token is always required; an empty
		// configured token does not turn authentication off.
		r.Group(func(r chi.Router) {
			r.Use(s.bearerAuthMiddleware)

			// Organizations
			r.Get("/organizations/{org}/entitlement-set", s.entitlementSetHandler)
			r.Get("/organizations/{org}/workspaces/{name}", s.getWorkspaceHandler)
			r.Post("/organizations/{org}/workspaces", s.createWorkspaceHandler)
			r.Get("/organizations/{org}/workspaces", s.listWorkspacesHandler)

			// Workspace update (by ID). OpenTofu's cloud backend PATCHes the
			// terraform-version after auto-creating a workspace.
			r.Patch("/workspaces/{id}", s.updateWorkspaceHandler)

			// Workspace actions (by ID)
			r.Post("/workspaces/{id}/actions/lock", s.lockWorkspaceHandler)
			r.Post("/workspaces/{id}/actions/unlock", s.unlockWorkspaceHandler)
			r.Post("/workspaces/{id}/actions/force-unlock", s.unlockWorkspaceHandler)

			// State versions (creation requires auth)
			r.Post("/workspaces/{id}/state-versions", s.createStateVersionHandler)
			r.Get("/workspaces/{id}/current-state-version", s.currentStateVersionHandler)

			// State version details
			r.Get("/state-versions/{id}", s.getStateVersionHandler)
		})

		// Auth-exempt routes (foreign upload/download requests)
		r.Put("/state-versions/{id}/state-upload", s.uploadStateHandler)
		r.Put("/state-versions/{id}/json-state-upload", s.uploadJSONStateHandler)
		r.Get("/state-versions/{id}/download", s.downloadStateHandler)
	})

	return r
}

// apiVersionMiddleware injects TFP-API-Version header on all /api/v2 responses.
func apiVersionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v2") {
			w.Header().Set("TFP-API-Version", "2.5")
		}
		next.ServeHTTP(w, r)
	})
}

// bearerAuthMiddleware validates the Bearer token.
func (s *Server) bearerAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid Authorization header")
			return
		}
		if token != s.token {
			writeError(w, http.StatusUnauthorized, "Unauthorized", "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// JSON:API helpers
// ──────────────────────────────────────────────────────────────────────────────

// jsonAPIResponse writes a JSON:API data envelope.
func jsonAPIResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// jsonAPIListResponse writes a JSON:API data envelope with a list of items.
func jsonAPIListResponse(w http.ResponseWriter, status int, items []any, meta map[string]any) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	env := map[string]any{"data": items}
	if meta != nil {
		env["meta"] = meta
	}
	json.NewEncoder(w).Encode(env)
}

// writeError writes a JSON:API error response.
func writeError(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{
			{
				"status": fmt.Sprintf("%d", status),
				"title":  title,
				"detail": detail,
			},
		},
	})
}

// writeAPIError writes an APIError as a JSON:API error response.
func writeAPIError(w http.ResponseWriter, err *APIError) {
	writeError(w, err.Status, err.Title, err.Detail)
}

// ──────────────────────────────────────────────────────────────────────────────
// Discovery
// ──────────────────────────────────────────────────────────────────────────────

func (s *Server) discoveryHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/.well-known/terraform.json" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"state.v2": s.serverURL + "/api/v2/",
		"tfe.v2":   s.serverURL + "/api/v2/",
		"tfe.v2.1": s.serverURL + "/api/v2/",
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// Ping
// ──────────────────────────────────────────────────────────────────────────────

func (s *Server) pingHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "{}")
}

// ──────────────────────────────────────────────────────────────────────────────
// Entitlements
// ──────────────────────────────────────────────────────────────────────────────

func (s *Server) entitlementSetHandler(w http.ResponseWriter, r *http.Request) {
	jsonAPIResponse(w, http.StatusOK, map[string]any{
		"id":   "org",
		"type": "entitlement-sets",
		"attributes": map[string]bool{
			"operations": false,
		},
	})
}
