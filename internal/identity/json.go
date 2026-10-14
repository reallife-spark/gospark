package identity

import "encoding/json"

func marshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func unmarshalJSON(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
