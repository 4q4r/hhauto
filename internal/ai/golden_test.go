package ai

import (
	"context"
	"path/filepath"
	"testing"
)

func TestGoldenFixtures(t *testing.T) {
	results, err := RunGolden(context.Background(), filepath.Join("testdata", "golden"))
	if err != nil {
		t.Fatalf("RunGolden: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("no golden cases discovered")
	}
	for _, res := range results {
		if res.Err != nil {
			t.Errorf("%s: %v\n want: %s\n got:  %s", res.Name, res.Err, res.Want, res.Got)
		}
	}
}
