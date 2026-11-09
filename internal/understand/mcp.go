package understand

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ServeMCP runs a minimal stdio MCP server over the graph. It opens no socket
// and reads only the graph file it was given.
func ServeMCP(graphPath string, input io.Reader, output io.Writer) error {
	graph, err := LoadGraph(graphPath)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bufio.NewReader(input))
	encoder := json.NewEncoder(output)
	for {
		var request rpcRequest
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		notification := len(request.ID) == 0
		response := rpcResponse{JSONRPC: "2.0", ID: request.ID}
		switch request.Method {
		case "initialize":
			response.Result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "sparknet-understand", "version": "0.1.0"},
			}
		case "notifications/initialized":
			continue
		case "tools/list":
			response.Result = map[string]any{"tools": tools()}
		case "tools/call":
			result, callErr := callTool(graph, request.Params)
			if callErr != nil {
				response.Error = &rpcError{Code: -32602, Message: callErr.Error()}
			} else {
				response.Result = result
			}
		default:
			response.Error = &rpcError{Code: -32601, Message: "method not found: " + request.Method}
		}
		if notification {
			continue
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
}

func tools() []map[string]any {
	return []map[string]any{
		{
			"name":        "understand_query",
			"description": "Search the code-understanding graph by text.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text":  map[string]any{"type": "string"},
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				},
			},
		},
		{
			"name":        "understand_node",
			"description": "Return one node and its neighbours.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string"}},
				"required":   []string{"id"},
			},
		},
		{
			"name":        "understand_path",
			"description": "Find a shortest path between two nodes.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"source": map[string]any{"type": "string"},
					"target": map[string]any{"type": "string"},
				},
				"required": []string{"source", "target"},
			},
		},
	}
}

func callTool(graph Graph, raw json.RawMessage) (map[string]any, error) {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return nil, err
	}
	var payload any
	switch call.Name {
	case "understand_query":
		text, _ := call.Arguments["text"].(string)
		limit := 20
		if value, ok := call.Arguments["limit"].(float64); ok {
			limit = int(value)
		}
		payload = Query(graph, text, limit)
	case "understand_node":
		id, _ := call.Arguments["id"].(string)
		node, ok := NodeByID(graph, id)
		if !ok {
			return nil, fmt.Errorf("node not found: %s", id)
		}
		payload = map[string]any{"node": node, "neighbors": Neighbors(graph, id, "both")}
	case "understand_path":
		source, _ := call.Arguments["source"].(string)
		target, _ := call.Arguments["target"].(string)
		path := ShortestPath(graph, source, target)
		if path == nil {
			return nil, fmt.Errorf("no path from %s to %s", source, target)
		}
		payload = path
	default:
		return nil, fmt.Errorf("unknown tool: %s", call.Name)
	}
	text, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(text)}}, "isError": false}, nil
}
