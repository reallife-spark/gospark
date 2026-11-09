package understand

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Parse runs the Python core's build command. The Go service deliberately does
// not reimplement the graph merger: the Python core is the one parser and the
// one merger, and this is the seam the ADR names.
func Parse(ctx context.Context, python, script, root, out string) error {
	if python == "" {
		python = "python3"
	}
	if script == "" {
		return fmt.Errorf("understand: no shared script path; set --shared or SPARKNET_SHARED_SCRIPT")
	}
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("understand: shared script %s: %w", script, err)
	}
	command := exec.CommandContext(ctx, python, script, "build", "--root", root, "--out", out)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("understand: python build: %w", err)
	}
	return nil
}

// DefaultSharedScript finds the fabric's loader relative to the working
// directory or a checkout. It returns the empty string when neither exists.
func DefaultSharedScript() string {
	if value := os.Getenv("SPARKNET_SHARED_SCRIPT"); value != "" {
		return value
	}
	for _, candidate := range []string{
		filepath.Join("shared", "sparknet-understand.py"),
		filepath.Join("..", "shared", "sparknet-understand.py"),
		filepath.Join("..", "..", "shared", "sparknet-understand.py"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}
