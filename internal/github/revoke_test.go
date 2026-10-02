package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func withApplicationsServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := applicationsAPIBase
	applicationsAPIBase = srv.URL + "/applications"
	t.Cleanup(func() { applicationsAPIBase = old })
}

func TestRevokeGrant_SendsTheDocumentedRequest(t *testing.T) {
	var method, path, user, pass, token string
	withApplicationsServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		user, pass, _ = r.BasicAuth()
		var b struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		token = b.AccessToken
		w.WriteHeader(204)
	})
	if err := RevokeGrant(context.Background(), "cid", "secret", "gho_tok"); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	if method != "DELETE" || path != "/applications/cid/grant" {
		t.Errorf("request = %s %s, want DELETE /applications/cid/grant", method, path)
	}
	if user != "cid" || pass != "secret" || token != "gho_tok" {
		t.Errorf("auth %q:%q token %q", user, pass, token)
	}
}

func TestRevokeGrant_AlreadyRevokedIsItsOwnError(t *testing.T) {
	withApplicationsServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	if err := RevokeGrant(context.Background(), "cid", "s", "t"); !errors.Is(err, ErrGrantNotFound) {
		t.Errorf("err = %v, want ErrGrantNotFound", err)
	}
}

func TestRevokeGrant_FailureIsAnError(t *testing.T) {
	withApplicationsServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	err := RevokeGrant(context.Background(), "cid", "s", "t")
	if err == nil || errors.Is(err, ErrGrantNotFound) {
		t.Errorf("err = %v, want a plain failure", err)
	}
	if err := RevokeGrant(context.Background(), "", "", "t"); err == nil {
		t.Error("unconfigured app: want error")
	}
}
