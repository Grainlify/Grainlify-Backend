package github

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"
)

type mockTransport struct {
	roundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTripFunc(req)
}

func TestAssignees_ConflictHandling(t *testing.T) {
	client := NewClient()

	t.Run("double-assign no error", func(t *testing.T) {
		client.HTTP.Transport = &mockTransport{
			roundTripFunc: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusCreated,
					Body:       io.NopCloser(bytes.NewBufferString(`{"assignees":[{"login":"user"}]}`)),
				}, nil
			},
		}

		applied, err := client.AddIssueAssignees(context.Background(), "token", "owner/repo", 1, []string{"user"})
		if err != nil {
			t.Fatalf("expected no error for idempotent assignment, got %v", err)
		}
		if len(applied) != 1 || applied[0] != "user" {
			t.Fatalf("expected applied assignees [user], got %v", applied)
		}
	})

	t.Run("double-remove no error", func(t *testing.T) {
		client.HTTP.Transport = &mockTransport{
			roundTripFunc: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewBufferString(`{}`)),
				}, nil
			},
		}

		err := client.RemoveIssueAssignees(context.Background(), "token", "owner/repo", 1, []string{"user"})
		if err != nil {
			t.Fatalf("expected no error for idempotent removal, got %v", err)
		}
	})

	t.Run("assignee lacking repo access", func(t *testing.T) {
		client.HTTP.Transport = &mockTransport{
			roundTripFunc: func(req *http.Request) (*http.Response, error) {
				errBody := `{"message":"Validation Failed","documentation_url":"https://docs.github.com/rest/issues/assignees"}`
				return &http.Response{
					StatusCode: http.StatusUnprocessableEntity,
					Body:       io.NopCloser(bytes.NewBufferString(errBody)),
				}, nil
			},
		}

		_, err := client.AddIssueAssignees(context.Background(), "token", "owner/repo", 1, []string{"invalid_user"})
		if err == nil {
			t.Fatal("expected error for invalid assignee, got nil")
		}

		fiberErr, ok := err.(*fiber.Error)
		if !ok {
			t.Fatalf("expected *fiber.Error, got %T: %v", err, err)
		}
		if fiberErr.Code != fiber.StatusUnprocessableEntity {
			t.Errorf("expected fiber.StatusUnprocessableEntity (422), got %d", fiberErr.Code)
		}
		if fiberErr.Message != "Validation Failed" {
			t.Errorf("expected 'Validation Failed', got %q", fiberErr.Message)
		}
	})

	// GitHub returns 201 and silently omits a requested login from the
	// resulting assignees array when it isn't a valid assignee (not a
	// collaborator, insufficient permissions, doesn't exist) -- it does not
	// return an error for this case. Callers must detect the omission
	// themselves by checking the returned list (see issue #293).
	t.Run("silently dropped assignee is surfaced in the returned list", func(t *testing.T) {
		client.HTTP.Transport = &mockTransport{
			roundTripFunc: func(req *http.Request) (*http.Response, error) {
				// Requested "no-access-user" but GitHub only applied "existing-assignee".
				return &http.Response{
					StatusCode: http.StatusCreated,
					Body:       io.NopCloser(bytes.NewBufferString(`{"assignees":[{"login":"existing-assignee"}]}`)),
				}, nil
			},
		}

		applied, err := client.AddIssueAssignees(context.Background(), "token", "owner/repo", 1, []string{"no-access-user"})
		if err != nil {
			t.Fatalf("expected no error (GitHub reports 2xx success even when dropping the login), got %v", err)
		}
		for _, login := range applied {
			if login == "no-access-user" {
				t.Fatalf("expected no-access-user to be absent from applied assignees, got %v", applied)
			}
		}
		if len(applied) != 1 || applied[0] != "existing-assignee" {
			t.Fatalf("expected applied assignees [existing-assignee], got %v", applied)
		}
	})
}
