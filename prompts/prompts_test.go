package prompts

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	s, err := Render("v1", "  I study gait sensors.  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "Reader profile:\nI study gait sensors.\n") {
		t.Errorf("profile not filled in:\n%s", s)
	}
	if strings.Contains(s, "{{PROFILE}}") {
		t.Error("placeholder left in prompt")
	}
}

func TestRenderUnknownVersion(t *testing.T) {
	if _, err := Render("v99", "x"); err == nil {
		t.Error("expected an error for an unknown version")
	}
}
