package understand

import "context"

// Diff asks the Python producer for the graph delta. The Go package keeps no
// second diff algebra: the returned bytes are Python's JSON result.
func Diff(ctx context.Context, python, script, oldPath, newPath string) ([]byte, error) {
	data, _, err := runPython(ctx, python, script, "diff", "--old", oldPath, "--new", newPath)
	return data, err
}
