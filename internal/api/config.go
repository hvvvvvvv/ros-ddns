package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"ros-ddns/internal/app"
	"ros-ddns/internal/config"
	"ros-ddns/internal/service"
)

const maxConfigBody = 1 << 20

// apiTokenMask replaces the raw API token in configuration documents returned
// over HTTP. PUT restores the current on-disk token before validation, so the
// read-only form can keep the secret while the browser never receives it.
const apiTokenMask = "********"

func registerConfig(mux *http.ServeMux, editor ConfigEditor) {
	available := func(w http.ResponseWriter) bool {
		if editor == nil {
			writeError(w, http.StatusServiceUnavailable, "configuration unavailable")
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		if !available(w) {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		document, err := editor.ConfigDocument()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot read configuration")
			return
		}
		writeJSON(w, http.StatusOK, publicConfigDocument(document))
	})
	mux.HandleFunc("POST /api/v1/config/validate", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data json.RawMessage `json:"data"`
		}
		if !readConfigJSON(w, r, &body) || !configObject(w, body.Data) || !available(w) {
			return
		}
		data, err := restoreAPIToken(editor, body.Data)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot read current configuration")
			return
		}
		if err := editor.ValidateConfig(data); err != nil {
			// Configuration errors can echo literal credentials. Return a safe
			// summary; authenticated editors can correct fields client-side.
			writeJSON(w, http.StatusOK, struct {
				Valid  bool     `json:"valid"`
				Errors []string `json:"errors"`
			}{false, []string{"configuration validation failed"}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
	})
	mux.HandleFunc("PUT /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(w, r) {
			return
		}
		var body struct {
			BaseHash *string         `json:"baseHash"`
			Data     json.RawMessage `json:"data"`
		}
		if !readConfigJSON(w, r, &body) || !configObject(w, body.Data) {
			return
		}
		if body.BaseHash == nil {
			writeError(w, http.StatusBadRequest, "baseHash must be a string")
			return
		}
		if !available(w) {
			return
		}
		data, err := restoreAPIToken(editor, body.Data)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot read current configuration")
			return
		}
		snapshot, warnings, err := editor.ApplyConfig(r.Context(), *body.BaseHash, data)
		switch {
		case errors.Is(err, app.ErrConfigConflict):
			writeError(w, http.StatusConflict, app.ErrConfigConflict.Error())
		case errors.Is(err, app.ErrRestartRequired):
			writeError(w, http.StatusUnprocessableEntity, app.ErrRestartRequired.Error())
		case err != nil:
			// Validation has no exported error type. Recheck failed edits with
			// the same validator; do not return backend text that may echo secrets.
			if editor.ValidateConfig(data) != nil {
				writeError(w, http.StatusUnprocessableEntity, "configuration validation failed")
			} else {
				writeError(w, http.StatusInternalServerError, "cannot apply configuration")
			}
		default:
			if warnings == nil {
				warnings = []string{}
			}
			writeJSON(w, http.StatusOK, struct {
				Snapshot service.Snapshot `json:"snapshot"`
				Warnings []string         `json:"warnings"`
			}{snapshot, warnings})
		}
	})
}

func publicConfigDocument(document config.Document) config.Document {
	api, ok := document.Data["api"].(map[string]any)
	if !ok {
		return document
	}
	token, hasToken := api["token"]
	if !hasToken || token == nil {
		return document
	}
	if text, isString := token.(string); isString && text == "" {
		return document
	}
	data := make(map[string]any, len(document.Data))
	for key, value := range document.Data {
		data[key] = value
	}
	apiCopy := make(map[string]any, len(api))
	for key, value := range api {
		apiCopy[key] = value
	}
	// YAML can pass an unquoted numeric/bool scalar into the string token field;
	// classify by set/unset rather than by the untyped document value's Go type.
	apiCopy["token"] = apiTokenMask
	data["api"] = apiCopy
	document.Data = data
	return document
}

// restoreAPIToken replaces the read-only mask with the current raw value (a
// literal token, numeric scalar or an environment reference). A client can never
// use the mask to change the token; changing api.token still requires editing the
// file and restarting the daemon.
func restoreAPIToken(editor ConfigEditor, data []byte) ([]byte, error) {
	object, err := config.DecodeStructured(data)
	if err != nil {
		return nil, err
	}
	api, ok := object["api"].(map[string]any)
	if !ok {
		return data, nil
	}
	if token, _ := api["token"].(string); token != apiTokenMask {
		return data, nil
	}
	current, err := editor.ConfigDocument()
	if err != nil {
		return nil, err
	}
	currentAPI, _ := current.Data["api"].(map[string]any)
	rawToken, hasToken := currentAPI["token"]
	if hasToken {
		// A genuine YAML string token (environment reference or quoted literal)
		// is preserved verbatim. Untyped numeric/bool/timestamp scalars lose their
		// original spelling/type in Data; use the parsed effective token so the
		// restart comparison stays unchanged after canonical serialization.
		if current.APITokenString {
			api["token"] = rawToken
		} else if current.APITokenValue != "" {
			api["token"] = current.APITokenValue
		} else {
			api["token"] = rawToken
		}
	} else {
		delete(api, "token")
	}
	return json.Marshal(object)
}

func readConfigJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if len(r.Header.Values("Content-Type")) != 1 || err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(target)
	if err == nil {
		var extra json.RawMessage
		err = decoder.Decode(&extra)
		if err == io.EOF {
			return true
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "configuration body exceeds 1 MiB")
	} else {
		writeError(w, http.StatusBadRequest, "body must contain one JSON object with the expected fields")
	}
	return false
}

func configObject(w http.ResponseWriter, data json.RawMessage) bool {
	// The outer decoder already checked JSON syntax; preserve original numbers
	// and environment placeholders by passing the raw object to ConfigEditor.
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		writeError(w, http.StatusBadRequest, "data must be an object")
		return false
	}
	return true
}

func sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Trust a reverse proxy's scheme only when it explicitly supplies one; the
	// bearer header remains the primary CSRF boundary either way.
	if forwarded := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))); forwarded == "http" || forwarded == "https" {
		scheme = forwarded
	}
	if len(origins) == 1 {
		origin, err := url.Parse(origins[0])
		if err == nil && origin.Scheme == scheme &&
			origin.Host != "" && strings.EqualFold(origin.Host, r.Host) && origin.User == nil &&
			origin.Path == "" && origin.RawQuery == "" && !origin.ForceQuery && !strings.Contains(origins[0], "#") {
			return true
		}
	}
	writeError(w, http.StatusForbidden, "origin must match the request host and scheme")
	return false
}
