package main

import (
	"strings"
	"testing"
)

func TestNoTTYError(t *testing.T) {
	cases := []struct {
		prompt   string
		wantWhat string
	}{
		{"Pick issue: ", "Pick issue"},
		{"cd to rig: ", "cd to rig"},
		{"Review PR: ", "Review PR"},
	}
	for _, c := range cases {
		t.Run(c.prompt, func(t *testing.T) {
			msg := noTTYError(c.prompt).Error()
			if !strings.HasPrefix(msg, c.wantWhat+" picker") {
				t.Errorf("message %q should start with %q picker", msg, c.wantWhat)
			}
			// The whole point is to hand the user the escape hatch.
			if !strings.Contains(msg, "skip the picker") {
				t.Errorf("message %q should point at the explicit-argument escape hatch", msg)
			}
		})
	}
}
