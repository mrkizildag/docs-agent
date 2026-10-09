package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestActionFilesUpToDate(t *testing.T) {
	t.Parallel()

	outs, err := outputs()
	if err != nil {
		t.Fatalf("outputs() error = %v", err)
	}
	for _, o := range outs {
		t.Run(o.file, func(t *testing.T) {
			t.Parallel()

			got, err := os.ReadFile(filepath.Join("..", "..", "..", "action", o.file))
			if err != nil {
				t.Fatalf("read %s: %v", o.file, err)
			}
			if diff := cmp.Diff(string(o.content), string(got)); diff != "" {
				t.Errorf("%s is stale; run make generate (-want +got):\n%s", o.file, diff)
			}
		})
	}
}
