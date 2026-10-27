package relay

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hudl/claude-teams/internal/team"
)

const (
	maxRequestBytes = 64 << 10
	maxTextRunes    = 16000
	maxNameRunes    = 64
	maxKindRunes    = 32
	maxVersionRunes = 32
	defaultLimit    = 100
	maxLimit        = 500
	defaultKind     = "update"
)

var (
	sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	versionPattern   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)
)

type Handler struct {
	store *Store
	token string
	mux   *http.ServeMux
}

func NewHandler(store *Store, token string) *Handler {
	if token == "" {
		panic("relay: token must not be empty")
	}
	h := &Handler{store: store, token: token, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /healthz", h.health)
	h.mux.HandleFunc("POST /sessions", h.requireClient(h.createSession))
	h.mux.HandleFunc("GET /sessions/{session}", h.requireClient(h.sessionInfo))
	h.mux.HandleFunc("DELETE /sessions/{session}", h.requireClient(h.stopSession))
	h.mux.HandleFunc("POST /sessions/{session}/members", h.requireClient(h.join))
	h.mux.HandleFunc("POST /sessions/{session}/messages", h.requireClient(h.publish))
	h.mux.HandleFunc("GET /sessions/{session}/messages", h.requireClient(h.messages))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) requireClient(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(h.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		if !validVersion(clientVersion(r)) {
			writeError(w, http.StatusBadRequest, team.VersionHeader+" header must be the team-mcp version, like 0.1.0")
			return
		}
		next(w, r)
	}
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	var request team.CreateSessionRequest
	if !decodeJSON(w, r, &request) {
		return
	}

	name := strings.TrimSpace(request.Name)
	createdBy := strings.TrimSpace(request.CreatedBy)
	if utf8.RuneCountInString(name) > maxNameRunes {
		writeError(w, http.StatusBadRequest, "name must be at most 64 characters")
		return
	}
	if !validMemberName(createdBy) {
		writeError(w, http.StatusBadRequest, "createdBy must be 1-64 characters")
		return
	}

	created, err := h.store.Create(r.Context(), name, createdBy, clientVersion(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	log.Printf("session=%s created by=%s name=%q", created.Session.ID, createdBy, name)
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) sessionInfo(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := sessionFromPath(w, r)
	if !ok {
		return
	}
	info, err := h.store.Info(r.Context(), sessionID, memberToken(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *Handler) stopSession(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := sessionFromPath(w, r)
	if !ok {
		return
	}
	stopped, err := h.store.Stop(r.Context(), sessionID, memberToken(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	log.Printf("session=%s stopped messages=%d", sessionID, stopped.LatestID)
	writeJSON(w, http.StatusOK, stopped)
}

func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := sessionFromPath(w, r)
	if !ok {
		return
	}
	var request team.JoinRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	name := strings.TrimSpace(request.Name)
	if !validMemberName(name) {
		writeError(w, http.StatusBadRequest, "name must be 1-64 characters")
		return
	}

	joined, err := h.store.Join(r.Context(), sessionID, name, clientVersion(r), memberToken(r))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	log.Printf("session=%s joined name=%s", sessionID, name)
	writeJSON(w, http.StatusOK, joined)
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := sessionFromPath(w, r)
	if !ok {
		return
	}
	var request team.PublishRequest
	if !decodeJSON(w, r, &request) {
		return
	}

	kind := strings.TrimSpace(request.Kind)
	if kind == "" {
		kind = defaultKind
	}
	text := strings.TrimSpace(request.Text)

	switch {
	case utf8.RuneCountInString(kind) > maxKindRunes:
		writeError(w, http.StatusBadRequest, "kind must be at most 32 characters")
		return
	case text == "":
		writeError(w, http.StatusBadRequest, "text is required")
		return
	case utf8.RuneCountInString(text) > maxTextRunes:
		writeError(w, http.StatusBadRequest, "text must be at most 16000 characters")
		return
	}

	message, err := h.store.Append(r.Context(), sessionID, memberToken(r), kind, text)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	log.Printf("session=%s id=%d from=%s kind=%s chars=%d", sessionID, message.ID, message.From, kind, utf8.RuneCountInString(text))
	writeJSON(w, http.StatusCreated, team.PublishResponse{ID: message.ID})
}

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := sessionFromPath(w, r)
	if !ok {
		return
	}

	since, err := parseIntParam(r, "since", 0)
	if err != nil || since < 0 {
		writeError(w, http.StatusBadRequest, "since must be a non-negative integer")
		return
	}
	limit, err := parseIntParam(r, "limit", defaultLimit)
	if err != nil || limit < 1 || limit > maxLimit {
		writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
		return
	}

	page, err := h.store.Since(r.Context(), sessionID, memberToken(r), since, int(limit))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func sessionFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	sessionID := strings.ToLower(r.PathValue("session"))
	if !sessionIDPattern.MatchString(sessionID) {
		writeError(w, http.StatusBadRequest, "session id must be a UUID")
		return "", false
	}
	return sessionID, true
}

func memberToken(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(team.MemberTokenHeader))
}

func clientVersion(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(team.VersionHeader))
}

func validVersion(version string) bool {
	return utf8.RuneCountInString(version) <= maxVersionRunes && versionPattern.MatchString(version)
}

func validMemberName(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= maxNameRunes
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func parseIntParam(r *http.Request, name string, fallback int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSessionNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrNotMember):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrNameTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrTooManySessions):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		log.Printf("store error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, team.ErrorResponse{Error: message})
}
