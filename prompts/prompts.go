// Package prompts holds the versioned system prompts. Each version is its own
// file so prompt changes show up in diffs and eval results can name them.
package prompts

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed *.txt
var files embed.FS

// Render returns prompt version (e.g. "v1") with the reader profile filled in.
func Render(version, profile string) (string, error) {
	b, err := files.ReadFile(version + ".txt")
	if err != nil {
		return "", fmt.Errorf("unknown prompt version %q (prompts live in prompts/<version>.txt)", version)
	}
	return strings.ReplaceAll(string(b), "{{PROFILE}}", strings.TrimSpace(profile)), nil
}
