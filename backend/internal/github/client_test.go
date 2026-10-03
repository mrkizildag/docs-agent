package github_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	ghclient "github.com/mrkizildag/docs-agent/backend/internal/github"
)

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestCreateCheckRun(t *testing.T) {
	t.Parallel()

	var gotCheckRunAuth string
	var gotCheckRunBody map[string]any

	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if len(auth) < len("Bearer ") || auth[:len("Bearer ")] != "Bearer " {
			t.Errorf("access_tokens Authorization = %q, want Bearer <jwt>", auth)
		}

		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
			t.Errorf("write access_tokens response: %v", err)
		}
	})
	mux.HandleFunc("POST /repos/o/r/check-runs", func(w http.ResponseWriter, r *http.Request) {
		gotCheckRunAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotCheckRunBody); err != nil {
			t.Fatalf("decode check-runs body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if _, err := fmt.Fprint(w, `{"id":1}`); err != nil {
			t.Errorf("write check-runs response: %v", err)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := ghclient.NewClient(&http.Client{Timeout: 5 * time.Second}, 1, testPrivateKeyPEM(t), srv.URL)
	if err != nil {
		t.Fatalf("NewClient() = %v, want nil error", err)
	}

	run := gate.CheckRun{
		Name:       "docs-agent",
		HeadSHA:    "abc123",
		Conclusion: gate.ConclusionSuccess,
		Title:      "docs-agent tracer",
		Summary:    "Analysis not implemented yet.",
	}

	if err := client.CreateCheckRun(t.Context(), 99, "o", "r", run); err != nil {
		t.Fatalf("CreateCheckRun(%+v) = %v, want nil", run, err)
	}

	if gotCheckRunAuth != "token ghs_test" {
		t.Errorf("check-runs Authorization = %q, want %q", gotCheckRunAuth, "token ghs_test")
	}

	want := map[string]any{
		"name":       "docs-agent",
		"head_sha":   "abc123",
		"status":     "completed",
		"conclusion": "success",
		"output": map[string]any{
			"title":   "docs-agent tracer",
			"summary": "Analysis not implemented yet.",
		},
	}

	if diff := cmp.Diff(want, gotCheckRunBody); diff != "" {
		t.Errorf("check-runs body (-want +got):\n%s", diff)
	}
}

func TestWorkflowExists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		wantExists bool
		wantErr    bool
	}{
		{name: "present", status: http.StatusOK, wantExists: true},
		{name: "absent", status: http.StatusNotFound, wantExists: false},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
					t.Errorf("write access_tokens response: %v", err)
				}
			})
			mux.HandleFunc("GET /repos/o/r/contents/.github/workflows/docs-agent.yml", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					if _, err := fmt.Fprint(w, `{"type":"file","name":"docs-agent.yml","path":".github/workflows/docs-agent.yml"}`); err != nil {
						t.Errorf("write contents response: %v", err)
					}
				}
			})

			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			client, err := ghclient.NewClient(&http.Client{Timeout: 5 * time.Second}, 1, testPrivateKeyPEM(t), srv.URL)
			if err != nil {
				t.Fatalf("NewClient() = %v, want nil error", err)
			}

			exists, err := client.WorkflowExists(t.Context(), 99, "o", "r")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("WorkflowExists() = nil error, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("WorkflowExists() = %v, want nil error", err)
			}
			if exists != tc.wantExists {
				t.Errorf("WorkflowExists() = %v, want %v", exists, tc.wantExists)
			}
		})
	}
}

func TestCreateCheckRunConcurrentInstallations(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
			t.Errorf("write access_tokens response: %v", err)
		}
	})
	mux.HandleFunc("POST /repos/o/r/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if _, err := fmt.Fprint(w, `{"id":1}`); err != nil {
			t.Errorf("write check-runs response: %v", err)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := ghclient.NewClient(&http.Client{Timeout: 5 * time.Second}, 1, testPrivateKeyPEM(t), srv.URL)
	if err != nil {
		t.Fatalf("NewClient() = %v, want nil error", err)
	}

	run := gate.CheckRun{
		Name:       "docs-agent",
		HeadSHA:    "abc123",
		Conclusion: gate.ConclusionSuccess,
		Title:      "docs-agent tracer",
		Summary:    "Analysis not implemented yet.",
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, installationID := range []int64{1, 2} {
		wg.Add(1)
		go func(installationID int64) {
			defer wg.Done()
			if err := client.CreateCheckRun(t.Context(), installationID, "o", "r", run); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(installationID)
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("CreateCheckRun() = %v, want nil", err)
	}
}
