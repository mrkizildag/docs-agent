package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	t.Parallel()

	status := func(code int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachableURL := unreachable.URL
	unreachable.Close()

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "200 passes", url: status(http.StatusOK).URL},
		{name: "500 fails", url: status(http.StatusInternalServerError).URL, wantErr: true},
		{name: "204 fails", url: status(http.StatusNoContent).URL, wantErr: true},
		{name: "unreachable fails", url: unreachableURL, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := check(t.Context(), &http.Client{Timeout: time.Second}, tc.url)
			if (err != nil) != tc.wantErr {
				t.Errorf("check(%q) error = %v, wantErr %v", tc.url, err, tc.wantErr)
			}
		})
	}
}
