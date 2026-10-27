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
	defaultLimit    = 100
	maxLimit        = 500
	defaultKind     = "update"
)

var teamNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Handler struct {
	store *Store
	token string
	mux   *http.ServeMux
}

func NewHandler(store *Store, token string) *Handler {
	h := &Handler{store: store, token: token, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /healthz", h.health)
	h.mux.HandleFunc("POST /teams/{team}/messages", h.requireToken(h.publish))
	h.mux.HandleFunc("GET /teams/{team}/messages", h.requireToken(h.messages))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.token == "" {
			next(w, r)
			return
		}
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(h.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		next(w, r)
	}
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request) {
	teamName, ok := teamFromPath(w, r)
	if !ok {
		return
	}

	var request team.PublishRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	from := strings.TrimSpace(request.From)
	kind := strings.TrimSpace(request.Kind)
	if kind == "" {
		kind = defaultKind
	}
	text := strings.TrimSpace(request.Text)

	switch {
	case from == "" || utf8.RuneCountInString(from) > maxNameRunes:
		writeError(w, http.StatusBadRequest, "from must be 1-64 characters")
		return
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

	message := h.store.Append(teamName, from, kind, text)
	log.Printf("team=%s id=%d from=%s kind=%s chars=%d", teamName, message.ID, from, kind, utf8.RuneCountInString(text))
	writeJSON(w, http.StatusCreated, team.PublishResponse{ID: message.ID})
}

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	teamName, ok := teamFromPath(w, r)
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

	writeJSON(w, http.StatusOK, h.store.Since(teamName, since, int(limit)))
}

func teamFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	teamName := r.PathValue("team")
	if !teamNamePattern.MatchString(teamName) {
		writeError(w, http.StatusBadRequest, "team must be 1-64 letters, digits, '-' or '_'")
		return "", false
	}
	return teamName, true
}

func parseIntParam(r *http.Request, name string, fallback int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	return strconv.ParseInt(raw, 10, 64)
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
