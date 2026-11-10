package understand

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runPython runs the path-stable shared/sparknet-understand.py CLI. The Go
// service never imports an installed Python package: the script path is the
// seam, and an absent or failing producer is an explicit error.
func runPython(ctx context.Context, python, script string, args ...string) ([]byte, []byte, error) {
	if python == "" {
		python = "python3"
	}
	if script == "" {
		return nil, nil, fmt.Errorf("understand: no shared script path; set --shared or SPARKNET_SHARED_SCRIPT")
	}
	if _, err := os.Stat(script); err != nil {
		return nil, nil, fmt.Errorf("understand: shared script %s: %w", script, err)
	}
	command := exec.CommandContext(ctx, python, append([]string{script}, args...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("understand: python %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("understand: python %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}
