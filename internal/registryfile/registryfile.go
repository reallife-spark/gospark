// Package registryfile loads node registries and authorizations from disk. It
// keeps the on-disk shape (JSON) in one place so the control plane, the agent
// and sparknetctl agree on how a registry file is read.
package registryfile

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/reallife-spark/goSpark/internal/apiv1"
)

// Load reads a full NodeRegistry from path.
func Load(path string) (*apiv1.NodeRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var reg apiv1.NodeRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("registry file %s: %w", path, err)
	}
	return &reg, nil
}

// LoadAuthorizations reads the authorization list from path. It accepts either a
// bare JSON array of authorizations or a registry object with an
// "authorizations" field. Any signature on the file is ignored: the caller
// (typically the control plane) is expected to re-sign.
func LoadAuthorizations(path string) ([]apiv1.NodeAuthorization, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var reg apiv1.NodeRegistry
	if err := json.Unmarshal(data, &reg); err == nil && reg.Authorizations != nil {
		return reg.Authorizations, nil
	}
	var list []apiv1.NodeAuthorization
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("authorizations file %s: %w", path, err)
	}
	return list, nil
}
