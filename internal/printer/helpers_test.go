package printer

import (
	"os"
	"testing"
)

func writeEmpty(path string) error { return os.WriteFile(path, nil, 0o600) }

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
