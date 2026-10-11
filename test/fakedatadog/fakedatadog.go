// Package fakedatadog is a tiny fake of the two Datadog REST endpoints that
// hack/datadog-readonly-check.sh uses, so the script's logic can be tested
// repeatably without real keys or network access. It does not imitate Datadog's
// telemetry intake and says nothing about how the real service behaves: one
// smoke run with real keys is still needed before a release.
package fakedatadog

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
)

const (
	// ValidAPIKey is the only API key /api/v1/validate accepts.
	ValidAPIKey = "fakeapikey0000"
	// AppKeyLast4 is the last four characters of the application key the
	// scenarios list. Tests build their key so that it ends with these.
	AppKeyLast4 = "1234"

	pageSizeDefault = 10
	fillerKeys      = 100 // keys listed before ours in the Paginated scenario
)

// Scenario selects what the fake returns for the application key listing.
type Scenario int

// The scenarios cover every branch of the check script.
const (
	ReadOnly             Scenario = iota // one key with read-only scopes
	HasWriteScope                        // the key also has monitors_write
	Unscoped                             // scopes is null: inherits all of the owner's permissions
	ListForbidden                        // the listing returns 403
	KeyNotListed                         // our key is not in the list
	SharedLast4ReadOnly                  // two keys share the last four, both read-only
	SharedLast4OneWrites                 // two keys share the last four, one can write
	Paginated                            // our key is on the second page
)

type keyEntry struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Attributes keyAttrs `json:"attributes"`
}

type keyAttrs struct {
	Name   string   `json:"name"`
	Last4  string   `json:"last4"`
	Scopes []string `json:"scopes"`
}

func entry(id, name, last4 string, scopes []string) keyEntry {
	return keyEntry{ID: id, Type: "application_keys", Attributes: keyAttrs{Name: name, Last4: last4, Scopes: scopes}}
}

var readScopes = []string{"monitors_read", "logs_read_data", "dashboards_read"}

func keysFor(s Scenario) []keyEntry {
	ours := func(scopes []string) keyEntry { return entry("k-ours", "triage-agent", AppKeyLast4, scopes) }
	other := entry("k-other", "someone-else", "9999", []string{"monitors_write"})
	switch s {
	case HasWriteScope:
		return []keyEntry{other, ours(append([]string{"monitors_write"}, readScopes...))}
	case Unscoped:
		return []keyEntry{ours(nil)}
	case KeyNotListed:
		return []keyEntry{other}
	case SharedLast4ReadOnly:
		return []keyEntry{ours(readScopes), entry("k-twin", "twin", AppKeyLast4, []string{"monitors_read"})}
	case SharedLast4OneWrites:
		return []keyEntry{ours(readScopes), entry("k-twin", "twin", AppKeyLast4, []string{"monitors_read", "dashboards_write"})}
	case Paginated:
		keys := make([]keyEntry, 0, fillerKeys+1)
		for i := 0; i < fillerKeys; i++ {
			keys = append(keys, entry(fmt.Sprintf("k-%03d", i), "filler", fmt.Sprintf("%04d", i), []string{"monitors_read"}))
		}
		return append(keys, ours(readScopes))
	default: // ReadOnly
		return []keyEntry{other, ours(readScopes)}
	}
}

// New starts a fake Datadog API for the scenario. Close it when done.
func New(s Scenario) *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/validate", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("DD-API-KEY") != ValidAPIKey {
			writeJSON(w, http.StatusForbidden, map[string]any{"errors": []string{"Forbidden"}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"valid": true})
	})

	mux.HandleFunc("GET /api/v2/current_user/application_keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("DD-API-KEY") != ValidAPIKey || r.Header.Get("DD-APPLICATION-KEY") == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{"errors": []string{"Forbidden"}})
			return
		}
		if s == ListForbidden {
			writeJSON(w, http.StatusForbidden, map[string]any{"errors": []string{"Forbidden: missing permission"}})
			return
		}
		all := keysFor(s)
		size := atoiOr(r.URL.Query().Get("page[size]"), pageSizeDefault)
		page := atoiOr(r.URL.Query().Get("page[number]"), 0)
		start := min(page*size, len(all))
		end := min(start+size, len(all))
		writeJSON(w, http.StatusOK, map[string]any{"data": all[start:end]})
	})

	return httptest.NewServer(mux)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}
