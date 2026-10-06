package github_test

import (
	"net/http"
	"testing"
)

func TestMergeBase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{name: "merge base", body: `{"merge_base_commit":{"sha":"mb1"}}`, want: "mb1"},
		{name: "no merge base", body: `{}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			handleAccessToken(t, mux)
			mux.HandleFunc("GET /repos/o/r/compare/{spec}", func(w http.ResponseWriter, r *http.Request) {
				if got := r.PathValue("spec"); got != "tip...head" {
					t.Errorf("compare spec = %q, want tip...head", got)
				}
				writeJSON(t, w, http.StatusOK, tc.body)
			})
			client := newTestClient(t, mux)

			got, err := client.MergeBase(t.Context(), 99, "o", "r", "tip", "head")
			if (err != nil) != tc.wantErr {
				t.Fatalf("MergeBase() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("MergeBase() = %q, want %q", got, tc.want)
			}
		})
	}
}
