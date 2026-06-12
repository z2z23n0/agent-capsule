package main

import (
	"strings"
	"testing"
)

func TestShareCommandRemoved(t *testing.T) {
	err := run([]string{"share"})
	if err == nil || !strings.Contains(err.Error(), `unknown command "share"`) {
		t.Fatalf("expected share to be removed, got %v", err)
	}
}
