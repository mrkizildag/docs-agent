package config_test

import (
	"log/slog"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/docs-agent/backend/internal/config"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    config.Config
		wantErr bool
	}{
		{
			name: "defaults",
			env:  map[string]string{},
			want: config.Config{Addr: ":8080", LogLevel: slog.LevelInfo},
		},
		{
			name: "overrides",
			env:  map[string]string{"ADDR": ":9000", "LOG_LEVEL": "debug"},
			want: config.Config{Addr: ":9000", LogLevel: slog.LevelDebug},
		},
		{
			name:    "invalid log level",
			env:     map[string]string{"LOG_LEVEL": "loud"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ADDR", "")
			t.Setenv("LOG_LEVEL", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			got, err := config.Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Load() (-want +got):\n%s", diff)
			}
		})
	}
}
