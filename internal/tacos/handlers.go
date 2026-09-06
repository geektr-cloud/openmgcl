package tacos

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ──────────────────────────────────────────────────────────────────────────────
// Workspace handlers
// ──────────────────────────────────────────────────────────────────────────────

func (s *Server) getWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	org := chi.URLParam(r, "org")
	name := chi.URLParam(r, "name")

	ws, err := s.store.GetWorkspace(org, name)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	jsonAPIResponse(w, http.StatusOK, workspaceJSON(ws))
}

func (s *Server) createWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	org := chi.URLParam(r, "org")

	var body struct {
		Data struct {
			Attributes struct {
				Name            string `json:"name"`
				TerraformVersion string `json:"terraform-version"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "invalid JSON body")
		return
	}
	if body.Data.Attributes.Name == "" {
		writeError(w, http.StatusUnprocessableEntity, "Unprocessable Entity", "name is required")
		return
	}

	ws, err := s.store.CreateWorkspace(org, body.Data.Attributes.Name, body.Data.Attributes.TerraformVersion)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	jsonAPIResponse(w, http.StatusCreated, workspaceJSON(ws))
}

func (s *Server) listWorkspacesHandler(w http.ResponseWriter, r *http.Request) {
	org := chi.URLParam(r, "org")
	namePrefix := r.URL.Query().Get("search[name]")
	page, _ := strconv.Atoi(r.URL.Query().Get("page[number]"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page[size]"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 100
	}

	wss, total, err := s.store.ListWorkspaces(org, namePrefix, page, pageSize)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	items := make([]any, len(wss))
	for i := range wss {
		items[i] = workspaceJSON(&wss[i])
	}

	totalPages := int(math.Ceil(float64(total) / float64(pageSize)))
	if totalPages < 1 {
		totalPages = 1
	}
	prevPage := page - 1
	if prevPage < 1 {
		prevPage = 0
	}
	nextPage := page + 1
	if nextPage > totalPages {
		nextPage = 0
	}

	meta := map[string]any{
		"pagination": map[string]int{
			"current-page": page,
			"next-page":    nextPage,
			"prev-page":    prevPage,
			"total-pages":  totalPages,
			"total-count":  total,
		},
	}
	jsonAPIListResponse(w, http.StatusOK, items, meta)
}

// workspaceJSON returns a JSON:API envelope for a workspace.
func workspaceJSON(ws *Workspace) map[string]any {
	return map[string]any{
		"id":   ws.ID,
		"type": "workspaces",
		"attributes": map[string]any{
			"name":             ws.Name,
			"execution-mode":   ws.ExecutionMode,
			"terraform-version": ws.TerraformVersion,
			"locked":           ws.Locked,
			"created-at":       ws.CreatedAt,
			"permissions": map[string]bool{
				"can-lock":          true,
				"can-unlock":        true,
				"can-force-unlock":  true,
				"can-update":        true,
				"can-destroy":       true,
				"can-queue-run":     false,
				"can-queue-apply":   false,
				"can-queue-destroy": false,
				"can-read-settings": true,
				"can-update-variable": true,
			},
		},
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Lock / Unlock handlers
// ──────────────────────────────────────────────────────────────────────────────

func (s *Server) lockWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var body struct {
		Reason string `json:"reason"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	if err := s.store.LockWorkspace(id, body.Reason); err != nil {
		handleStoreError(w, err)
		return
	}

	ws, err := s.store.GetWorkspaceByID(id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	jsonAPIResponse(w, http.StatusOK, workspaceJSON(ws))
}

func (s *Server) unlockWorkspaceHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	if err := s.store.UnlockWorkspace(id); err != nil {
		handleStoreError(w, err)
		return
	}

	ws, err := s.store.GetWorkspaceByID(id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	jsonAPIResponse(w, http.StatusOK, workspaceJSON(ws))
}

// ──────────────────────────────────────────────────────────────────────────────
// State version handlers
// ──────────────────────────────────────────────────────────────────────────────

func (s *Server) createStateVersionHandler(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")

	var body struct {
		Data struct {
			Attributes struct {
				Serial   int    `json:"serial"`
				Lineage  string `json:"lineage"`
				MD5      string `json:"md5"`
				State    string `json:"state"` // base64-encoded, optional
				Force    bool   `json:"force"`
				JSONState        string `json:"json-state"`
				JSONStateOutputs string `json:"json-state-outputs"`
			} `json:"attributes"`
			Relationships map[string]any `json:"relationships"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "invalid JSON body")
		return
	}

	attrs := body.Data.Attributes

	// Decode inline state if present (fallback path)
	var stateBytes []byte
	if attrs.State != "" {
		var err error
		stateBytes, err = base64.StdEncoding.DecodeString(attrs.State)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "Unprocessable Entity", "invalid base64 state")
			return
		}
	}

	var jsonStateBytes, jsonStateOutputsBytes []byte
	if attrs.JSONState != "" {
		jsonStateBytes, _ = base64.StdEncoding.DecodeString(attrs.JSONState)
	}
	if attrs.JSONStateOutputs != "" {
		jsonStateOutputsBytes, _ = base64.StdEncoding.DecodeString(attrs.JSONStateOutputs)
	}

	sv, err := s.store.CreateStateVersion(
		wsID, attrs.Serial, attrs.Lineage, attrs.MD5,
		stateBytes, jsonStateBytes, jsonStateOutputsBytes,
		attrs.Force,
	)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	resp := stateVersionJSON(sv, s.serverURL)
	if sv.Status == "pending" {
		// Modern upload path: include upload URLs
		resp["attributes"].(map[string]any)["hosted-state-upload-url"] =
			fmt.Sprintf("%s/api/v2/state-versions/%s/state-upload", s.serverURL, sv.ID)
		resp["attributes"].(map[string]any)["hosted-json-state-upload-url"] =
			fmt.Sprintf("%s/api/v2/state-versions/%s/json-state-upload", s.serverURL, sv.ID)
	}
	jsonAPIResponse(w, http.StatusCreated, resp)
}

func (s *Server) currentStateVersionHandler(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")

	sv, err := s.store.GetCurrentStateVersion(wsID)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	jsonAPIResponse(w, http.StatusOK, stateVersionJSON(sv, s.serverURL))
}

func (s *Server) getStateVersionHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	sv, err := s.store.GetStateVersion(id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	jsonAPIResponse(w, http.StatusOK, stateVersionJSON(sv, s.serverURL))
}

// ──────────────────────────────────────────────────────────────────────────────
// Upload / Download handlers (auth-exempt)
// ──────────────────────────────────────────────────────────────────────────────

func (s *Server) uploadStateHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "failed to read body")
		return
	}
	if err := s.store.UploadState(id, data); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) uploadJSONStateHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Bad Request", "failed to read body")
		return
	}
	if err := s.store.UploadJSONState(id, data); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) downloadStateHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sv, err := s.store.GetStateVersion(id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if sv.Status != "finalized" || len(sv.State) == 0 {
		writeError(w, http.StatusNotFound, "Not Found", "state not available")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(sv.State)
}

// ──────────────────────────────────────────────────────────────────────────────
// JSON:API builders
// ──────────────────────────────────────────────────────────────────────────────

func stateVersionJSON(sv *StateVersion, serverURL string) map[string]any {
	attrs := map[string]any{
		"serial":   sv.Serial,
		"status":   sv.Status,
		"created-at": sv.CreatedAt,
	}
	if sv.Lineage != "" {
		attrs["lineage"] = sv.Lineage
	}
	if sv.MD5 != "" {
		attrs["md5"] = sv.MD5
	}
	if sv.Status == "finalized" {
		attrs["hosted-state-download-url"] = fmt.Sprintf("%s/api/v2/state-versions/%s/download", serverURL, sv.ID)
	}
	return map[string]any{
		"id":   sv.ID,
		"type": "state-versions",
		"attributes": attrs,
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Error handling
// ──────────────────────────────────────────────────────────────────────────────

func handleStoreError(w http.ResponseWriter, err error) {
	if apiErr, ok := err.(*APIError); ok {
		writeAPIError(w, apiErr)
		return
	}
	// Check for unique constraint violations that weren't caught as APIErrors
	errStr := err.Error()
	if strings.Contains(errStr, "UNIQUE") {
		writeError(w, http.StatusConflict, "Conflict", errStr)
		return
	}
	writeError(w, http.StatusInternalServerError, "Internal Server Error", errStr)
}
