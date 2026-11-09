package understand

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadGraph reads a graph from disk.
func LoadGraph(path string) (Graph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Graph{}, err
	}
	var graph Graph
	if err := json.Unmarshal(data, &graph); err != nil {
		return Graph{}, fmt.Errorf("understand: parse %s: %w", path, err)
	}
	return graph, nil
}

// SaveGraph writes a graph atomically enough for the console's read path:
// the parent is created and the file is replaced in one write.
func SaveGraph(path string, graph Graph) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(graph, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// NodeByID returns the node with the given id.
func NodeByID(graph Graph, id string) (Node, bool) {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return Node{}, false
}

// Query ranks nodes by a case-insensitive text match. An empty query returns
// the highest-degree nodes, which is a useful default for a dashboard.
func Query(graph Graph, text string, limit int) []Node {
	if limit <= 0 {
		limit = 20
	}
	terms := strings.Fields(strings.ToLower(text))
	scores := make(map[string]int, len(graph.Nodes))
	if len(terms) == 0 {
		for _, edge := range graph.Edges {
			scores[edge.Source]++
			scores[edge.Target]++
		}
	} else {
		for _, node := range graph.Nodes {
			haystack := strings.ToLower(node.ID + " " + node.Name + " " + node.Summary + " " + node.FilePath + " " + strings.Join(node.Tags, " "))
			for _, term := range terms {
				if strings.EqualFold(node.Name, term) {
					scores[node.ID] += 4
				} else if strings.HasPrefix(strings.ToLower(node.Name), term) {
					scores[node.ID] += 3
				} else if strings.Contains(haystack, term) {
					scores[node.ID]++
				}
			}
		}
	}
	matches := make([]Node, 0, len(scores))
	for _, node := range graph.Nodes {
		if scores[node.ID] > 0 {
			matches = append(matches, node)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if scores[matches[i].ID] != scores[matches[j].ID] {
			return scores[matches[i].ID] > scores[matches[j].ID]
		}
		return matches[i].ID < matches[j].ID
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

// Neighbors returns the ids adjacent to id. direction is "out", "in" or "both".
func Neighbors(graph Graph, id string, direction string) []string {
	found := map[string]struct{}{}
	for _, edge := range graph.Edges {
		switch direction {
		case "out":
			if edge.Source == id {
				found[edge.Target] = struct{}{}
			}
		case "in":
			if edge.Target == id {
				found[edge.Source] = struct{}{}
			}
		default:
			if edge.Source == id {
				found[edge.Target] = struct{}{}
			}
			if edge.Target == id {
				found[edge.Source] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(found))
	for value := range found {
		ids = append(ids, value)
	}
	sort.Strings(ids)
	return ids
}

// ShortestPath returns a BFS path from source to target, including both ends.
func ShortestPath(graph Graph, source, target string) []string {
	if source == target {
		return []string{source}
	}
	adjacency := map[string][]string{}
	for _, edge := range graph.Edges {
		adjacency[edge.Source] = append(adjacency[edge.Source], edge.Target)
	}
	previous := map[string]string{source: ""}
	queue := []string{source}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacency[current] {
			if _, seen := previous[next]; seen {
				continue
			}
			previous[next] = current
			if next == target {
				path := []string{target}
				for at := target; at != source; at = previous[at] {
					path = append(path, previous[at])
				}
				for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
					path[left], path[right] = path[right], path[left]
				}
				return path
			}
			queue = append(queue, next)
		}
	}
	return nil
}

// Diff reports node and edge changes between two graphs.
func Diff(oldGraph, newGraph Graph) DiffResult {
	oldNodes := map[string]Node{}
	for _, node := range oldGraph.Nodes {
		oldNodes[node.ID] = node
	}
	newNodes := map[string]Node{}
	for _, node := range newGraph.Nodes {
		newNodes[node.ID] = node
	}
	result := DiffResult{NodesAdded: []string{}, NodesRemoved: []string{}, NodesChanged: []string{}, EdgesAdded: [][]string{}, EdgesRemoved: [][]string{}}
	for id, node := range newNodes {
		oldNode, ok := oldNodes[id]
		if !ok {
			result.NodesAdded = append(result.NodesAdded, id)
			continue
		}
		oldJSON, _ := json.Marshal(oldNode)
		newJSON, _ := json.Marshal(node)
		if string(oldJSON) != string(newJSON) {
			result.NodesChanged = append(result.NodesChanged, id)
		}
	}
	for id := range oldNodes {
		if _, ok := newNodes[id]; !ok {
			result.NodesRemoved = append(result.NodesRemoved, id)
		}
	}
	oldEdges := map[string]Edge{}
	for _, edge := range oldGraph.Edges {
		oldEdges[edgeKey(edge)] = edge
	}
	newEdges := map[string]Edge{}
	for _, edge := range newGraph.Edges {
		newEdges[edgeKey(edge)] = edge
	}
	for key := range newEdges {
		if _, ok := oldEdges[key]; !ok {
			result.EdgesAdded = append(result.EdgesAdded, strings.Split(key, "\x00"))
		}
	}
	for key := range oldEdges {
		if _, ok := newEdges[key]; !ok {
			result.EdgesRemoved = append(result.EdgesRemoved, strings.Split(key, "\x00"))
		}
	}
	sort.Strings(result.NodesAdded)
	sort.Strings(result.NodesRemoved)
	sort.Strings(result.NodesChanged)
	sort.Slice(result.EdgesAdded, func(i, j int) bool {
		return strings.Join(result.EdgesAdded[i], "") < strings.Join(result.EdgesAdded[j], "")
	})
	sort.Slice(result.EdgesRemoved, func(i, j int) bool {
		return strings.Join(result.EdgesRemoved[i], "") < strings.Join(result.EdgesRemoved[j], "")
	})
	return result
}

func edgeKey(edge Edge) string { return edge.Source + "\x00" + edge.Target + "\x00" + edge.Type }

// ExportDOT renders a Graphviz document. The graph is data, so labels are
// escaped rather than interpreted.
func ExportDOT(graph Graph) string {
	var builder strings.Builder
	builder.WriteString("digraph understand {\n")
	for _, node := range graph.Nodes {
		fmt.Fprintf(&builder, "  %q [label=%q];\n", node.ID, node.Name)
	}
	for _, edge := range graph.Edges {
		fmt.Fprintf(&builder, "  %q -> %q [label=%q];\n", edge.Source, edge.Target, edge.Type)
	}
	builder.WriteString("}\n")
	return builder.String()
}

// ExportMermaid renders a Mermaid flowchart.
func ExportMermaid(graph Graph) string {
	var builder strings.Builder
	builder.WriteString("graph TD\n")
	for _, node := range graph.Nodes {
		fmt.Fprintf(&builder, "  %q[%q]\n", node.ID, node.Name)
	}
	for _, edge := range graph.Edges {
		fmt.Fprintf(&builder, "  %q -->|%s| %q\n", edge.Source, edge.Type, edge.Target)
	}
	return builder.String()
}
