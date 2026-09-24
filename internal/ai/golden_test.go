package ai

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGoldenFixtures(t *testing.T) {
	RunGolden(context.Background(), filepath.Join("testdata", "golden"), t)
}
