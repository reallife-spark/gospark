package understand

import (
	"context"
	"encoding/json"
	"fmt"
)

// Scan asks the Python producer for the deterministic file walk. Scan policy —
// default excludes, .understandignore, language maps, test paths, binary
// detection and the size cap — lives in shared/sparknet_understand, not here.
func Scan(ctx context.Context, python, script, root string, excludes []string) (ScanResult, error) {
	args := []string{"scan", "--root", root}
	for _, exclude := range excludes {
		args = append(args, "--exclude", exclude)
	}
	data, _, err := runPython(ctx, python, script, args...)
	if err != nil {
		return ScanResult{}, err
	}
	var result ScanResult
	if err := json.Unmarshal(data, &result); err != nil {
		return ScanResult{}, fmt.Errorf("understand: python scan output: %w", err)
	}
	return result, nil
}
