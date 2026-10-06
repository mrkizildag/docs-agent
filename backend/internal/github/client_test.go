package github_test

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
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

	client := newTestClient(t, mux)

	run := gate.CheckRun{
		Name:       "pollux-agent",
		HeadSHA:    "abc123",
		Conclusion: gate.ConclusionSuccess,
		Title:      "pollux-agent tracer",
		Summary:    "Analysis not implemented yet.",
	}

	id, err := client.CreateCheckRun(t.Context(), 99, "o", "r", run)
	if err != nil {
		t.Fatalf("CreateCheckRun(%+v) = %v, want nil", run, err)
	}
	if id != 1 {
		t.Errorf("CreateCheckRun() id = %d, want 1", id)
	}

	if gotCheckRunAuth != "token ghs_test" {
		t.Errorf("check-runs Authorization = %q, want %q", gotCheckRunAuth, "token ghs_test")
	}

	want := map[string]any{
		"name":       "pollux-agent",
		"head_sha":   "abc123",
		"status":     "completed",
		"conclusion": "success",
		"output": map[string]any{
			"title":   "pollux-agent tracer",
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
			handleAccessToken(t, mux)
			mux.HandleFunc("GET /repos/o/r/contents/.github/workflows/pollux-agent.yml", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					if _, err := fmt.Fprint(w, `{"type":"file","name":"pollux-agent.yml","path":".github/workflows/pollux-agent.yml"}`); err != nil {
						t.Errorf("write contents response: %v", err)
					}
				}
			})

			client := newTestClient(t, mux)

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

func TestInstallationToken(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	var body map[string]any
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode access_tokens body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
			t.Errorf("write access_tokens response: %v", err)
		}
	})

	client := newTestClient(t, mux)

	token, err := client.InstallationToken(t.Context(), 99, "r")
	if err != nil {
		t.Fatalf("InstallationToken() = %v, want nil error", err)
	}
	if token != "ghs_test" {
		t.Errorf("InstallationToken() = %q, want %q", token, "ghs_test")
	}

	wantBody := map[string]any{
		"repositories": []any{"r"},
		"permissions":  map[string]any{"contents": "read"},
	}
	if diff := cmp.Diff(wantBody, body); diff != "" {
		t.Errorf("access_tokens body (-want +got):\n%s", diff)
	}
}

func TestCreateCheckRunConcurrentInstallations(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("POST /repos/o/r/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if _, err := fmt.Fprint(w, `{"id":1}`); err != nil {
			t.Errorf("write check-runs response: %v", err)
		}
	})

	client := newTestClient(t, mux)

	run := gate.CheckRun{
		Name:       "pollux-agent",
		HeadSHA:    "abc123",
		Conclusion: gate.ConclusionSuccess,
		Title:      "pollux-agent tracer",
		Summary:    "Analysis not implemented yet.",
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, installationID := range []int64{1, 2} {
		wg.Add(1)
		go func(installationID int64) {
			defer wg.Done()
			if _, err := client.CreateCheckRun(t.Context(), installationID, "o", "r", run); err != nil {
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

func handleAccessToken(t *testing.T, mux *http.ServeMux) {
	t.Helper()

	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
			t.Errorf("write access_tokens response: %v", err)
		}
	})
}

func newTestClient(t *testing.T, mux *http.ServeMux) *ghclient.Client {
	t.Helper()

	client, _ := newTestClientURL(t, mux)
	return client
}

// newTestClientURL also returns the test server's URL, for handlers that redirect back to it.
func newTestClientURL(t *testing.T, mux *http.ServeMux) (*ghclient.Client, string) {
	t.Helper()

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := ghclient.NewClient(&http.Client{Timeout: 5 * time.Second}, 1, testPrivateKeyPEM(t), srv.URL)
	if err != nil {
		t.Fatalf("NewClient() = %v, want nil error", err)
	}
	return client, srv.URL
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := fmt.Fprint(w, body); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestCreateAndUpdateCheckRunInProgress(t *testing.T) {
	t.Parallel()

	var created, updated map[string]any
	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("POST /repos/o/r/check-runs", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		writeJSON(t, w, http.StatusCreated, `{"id":77}`)
	})
	mux.HandleFunc("PATCH /repos/o/r/check-runs/77", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			t.Errorf("decode update body: %v", err)
		}
		writeJSON(t, w, http.StatusOK, `{"id":77}`)
	})
	client := newTestClient(t, mux)

	run := gate.CheckRun{Name: "pollux-agent", HeadSHA: "abc", Status: gate.StatusInProgress, Title: "t", Summary: "s"}
	id, err := client.CreateCheckRun(t.Context(), 99, "o", "r", run)
	if err != nil || id != 77 {
		t.Fatalf("CreateCheckRun() = %d, %v, want 77, nil", id, err)
	}
	wantCreated := map[string]any{
		"name": "pollux-agent", "head_sha": "abc", "status": "in_progress",
		"output": map[string]any{"title": "t", "summary": "s"},
	}
	if diff := cmp.Diff(wantCreated, created); diff != "" {
		t.Errorf("create body (-want +got):\n%s", diff)
	}

	done := gate.CheckRun{Name: "pollux-agent", HeadSHA: "abc", Status: gate.StatusCompleted, Conclusion: gate.ConclusionActionRequired, Title: "t2", Summary: "s2"}
	if err := client.UpdateCheckRun(t.Context(), 99, "o", "r", 77, done); err != nil {
		t.Fatalf("UpdateCheckRun() = %v, want nil", err)
	}
	wantUpdated := map[string]any{
		"name": "pollux-agent", "status": "completed", "conclusion": "action_required",
		"output": map[string]any{"title": "t2", "summary": "s2"},
	}
	if diff := cmp.Diff(wantUpdated, updated); diff != "" {
		t.Errorf("update body (-want +got):\n%s", diff)
	}
}

func TestDispatch(t *testing.T) {
	t.Parallel()

	var got map[string]any
	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"default_branch":"trunk"}`)
	})
	mux.HandleFunc("POST /repos/o/r/actions/workflows/pollux-agent.yml/dispatches", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode dispatch body: %v", err)
		}
		writeJSON(t, w, http.StatusOK, `{"workflow_run_id":4242,"run_url":"u","html_url":"h"}`)
	})
	client := newTestClient(t, mux)

	runID, err := client.Dispatch(t.Context(), 99, "o", "r", actions.DispatchInputs{HeadSHA: "abc", PRNumber: 7, Nonce: "n1", Docs: []string{"docs/a.md"}})
	if err != nil || runID != 4242 {
		t.Fatalf("Dispatch() = %d, %v, want 4242, nil", runID, err)
	}

	want := map[string]any{
		"ref":                "trunk",
		"inputs":             map[string]any{"head_sha": "abc", "pr_number": "7", "nonce": "n1", "docs": `["docs/a.md"]`},
		"return_run_details": true,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("dispatch body (-want +got):\n%s", diff)
	}
}

func TestResultArtifact(t *testing.T) {
	t.Parallel()

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	f, err := zw.Create("result.json")
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := f.Write([]byte(`{"head_sha":"abc"}`)); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	var (
		client    *ghclient.Client
		serverURL string
	)
	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("GET /repos/o/r/actions/runs/4242/artifacts", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"total_count":2,"artifacts":[{"id":1,"name":"pollux-agent-result","workflow_run":{"id":7}},{"id":3,"name":"other","workflow_run":{"id":4242}},{"id":2,"name":"pollux-agent-result","workflow_run":{"id":4242}}]}`)
	})
	mux.HandleFunc("GET /repos/o/r/actions/artifacts/2/zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, serverURL+"/blob", http.StatusFound)
	})
	mux.HandleFunc("GET /blob", func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("blob download Authorization = %q, want none", auth)
		}
		if _, err := w.Write(archive.Bytes()); err != nil {
			t.Errorf("write blob: %v", err)
		}
	})
	client, serverURL = newTestClientURL(t, mux)

	got, err := client.ResultArtifact(t.Context(), 99, "o", "r", 4242)
	if err != nil {
		t.Fatalf("ResultArtifact() = %v, want nil", err)
	}
	if string(got) != `{"head_sha":"abc"}` {
		t.Errorf("ResultArtifact() = %q, want the result.json bytes", got)
	}
}

func zipOf(t *testing.T, name string, content []byte) []byte {
	t.Helper()

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	f, err := zw.Create(name)
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return archive.Bytes()
}

func TestResultArtifactFailures(t *testing.T) {
	t.Parallel()

	const capBytes = 10 << 20
	const listed = `{"total_count":1,"artifacts":[{"id":2,"name":"pollux-agent-result","workflow_run":{"id":4242}}]}`

	tests := []struct {
		name         string
		artifacts    string
		downloadCode int
		blob         []byte
		wantErr      string
	}{
		{name: "zip over the cap", artifacts: listed, downloadCode: http.StatusOK, blob: make([]byte, capBytes+1), wantErr: "larger than"},
		{name: "result.json over the cap", artifacts: listed, downloadCode: http.StatusOK, blob: zipOf(t, "result.json", make([]byte, capBytes+1)), wantErr: "read result.json in artifact: larger than"},
		{name: "non-200 download", artifacts: listed, downloadCode: http.StatusForbidden, wantErr: "status 403"},
		{name: "missing artifact", artifacts: `{"total_count":0,"artifacts":[]}`, wantErr: "no pollux-agent-result artifact"},
		{name: "expired artifact", artifacts: `{"total_count":1,"artifacts":[{"id":2,"name":"pollux-agent-result","expired":true,"workflow_run":{"id":4242}}]}`, wantErr: "no pollux-agent-result artifact"},
		{name: "zip without result.json", artifacts: listed, downloadCode: http.StatusOK, blob: zipOf(t, "other.json", []byte("{}")), wantErr: "open result.json in artifact"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				client    *ghclient.Client
				serverURL string
			)
			mux := http.NewServeMux()
			handleAccessToken(t, mux)
			mux.HandleFunc("GET /repos/o/r/actions/runs/4242/artifacts", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, tc.artifacts)
			})
			mux.HandleFunc("GET /repos/o/r/actions/artifacts/2/zip", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, serverURL+"/blob", http.StatusFound)
			})
			mux.HandleFunc("GET /blob", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.downloadCode)
				if _, err := w.Write(tc.blob); err != nil {
					t.Errorf("write blob: %v", err)
				}
			})
			client, serverURL = newTestClientURL(t, mux)

			got, err := client.ResultArtifact(t.Context(), 99, "o", "r", 4242)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ResultArtifact() = %q, %v; want an error containing %q", got, err, tc.wantErr)
			}
		})
	}
}
