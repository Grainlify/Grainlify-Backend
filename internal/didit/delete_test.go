package didit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withDeleteServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := deleteBaseURL
	deleteBaseURL = srv.URL + "/v3"
	t.Cleanup(func() { deleteBaseURL = old })
	return NewClient("key-123")
}

func TestDeleteSession_SendsTheDocumentedRequest(t *testing.T) {
	var gotMethod, gotPath, gotKey string
	c := withDeleteServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotKey = r.Method, r.URL.Path, r.Header.Get("x-api-key")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"face_retention_outcome":"deleted"}`))
	})
	if err := c.DeleteSession(context.Background(), "abc-123"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if gotMethod != "DELETE" || gotPath != "/v3/session/abc-123/delete/" || gotKey != "key-123" {
		t.Errorf("request = %s %s key=%q", gotMethod, gotPath, gotKey)
	}
}

// Already gone is what an erasure wants, so it is distinguishable from a
// failure - and a retry of a half-finished erasure does not get stuck on it.
func TestDeleteSession_NotFoundIsItsOwnError(t *testing.T) {
	c := withDeleteServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	if err := c.DeleteSession(context.Background(), "gone"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

// A refusal must not put Didit's body into the error: errors are logged, and
// the body is about a person's identity session.
func TestDeleteSession_FailureDoesNotEchoTheBody(t *testing.T) {
	c := withDeleteServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"first_name":"Ada","document_number":"X123"}`))
	})
	err := c.DeleteSession(context.Background(), "s")
	if err == nil {
		t.Fatal("want an error for 403")
	}
	if strings.Contains(err.Error(), "Ada") || strings.Contains(err.Error(), "X123") {
		t.Errorf("error echoes the response body: %v", err)
	}
	if !strings.Contains(err.Error(), "delete:sessions") {
		t.Errorf("error should point at the missing privilege: %v", err)
	}
}

func TestDeleteSession_RefusesWithoutKeyOrID(t *testing.T) {
	if err := NewClient("").DeleteSession(context.Background(), "s"); err == nil {
		t.Error("no key: want error")
	}
	if err := NewClient("k").DeleteSession(context.Background(), ""); err == nil {
		t.Error("no session id: want error")
	}
}
