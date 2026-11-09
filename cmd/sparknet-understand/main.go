// Command sparknet-understand is the Go control/service surface for the
// Understand-Anything graph: scan, parse through the Python seam, query, diff,
// export and a stdio MCP server. It opens no network port; the node console
// reads the graph from `.ua/` and the MCP server speaks over stdio.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reallife-spark/goSpark/internal/understand"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sparknet-understand:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "scan":
		return runScan(args[1:])
	case "parse":
		return runParse(args[1:])
	case "query":
		return runQuery(args[1:])
	case "diff":
		return runDiff(args[1:])
	case "export":
		return runExport(args[1:])
	case "gofrag":
		return runGoFrag(args[1:])
	case "mcp":
		return runMCP(args[1:])
	case "version", "--version", "-version":
		fmt.Println("sparknet-understand", version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (try help)", args[0])
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `sparknet-understand — code-understanding graph service

Usage:
  sparknet-understand scan    --root DIR [--out FILE] [--exclude PATTERN]...
  sparknet-understand parse   --root DIR [--out FILE] [--python PATH] [--shared PATH]
  sparknet-understand gofrag  --root DIR [--out FILE]
  sparknet-understand query   --graph FILE [--text TEXT] [--limit N]
  sparknet-understand diff    --old FILE --new FILE [--out FILE]
  sparknet-understand export  --graph FILE [--format json|dot|mermaid] [--out FILE]
  sparknet-understand mcp     --graph FILE
  sparknet-understand version`)
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func runScan(args []string) error {
	flags := flag.NewFlagSet("scan", flag.ExitOnError)
	root := flags.String("root", ".", "tree to scan")
	out := flags.String("out", "", "write JSON here instead of stdout")
	var excludes stringList
	flags.Var(&excludes, "exclude", "additional ignore pattern (repeatable)")
	_ = flags.Parse(args)
	result, err := understand.Scan(*root, excludes)
	if err != nil {
		return err
	}
	return writeJSON(*out, result)
}

func runParse(args []string) error {
	flags := flag.NewFlagSet("parse", flag.ExitOnError)
	root := flags.String("root", ".", "tree to parse")
	out := flags.String("out", "", "graph output (default <root>/.ua/knowledge-graph.json)")
	python := flags.String("python", "python3", "Python interpreter")
	shared := flags.String("shared", understand.DefaultSharedScript(), "path to shared/sparknet-understand.py")
	_ = flags.Parse(args)
	destination := *out
	if destination == "" {
		destination = filepath.Join(*root, ".ua", "knowledge-graph.json")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	return understand.Parse(context.Background(), *python, *shared, *root, destination)
}

func runQuery(args []string) error {
	flags := flag.NewFlagSet("query", flag.ExitOnError)
	graphPath := flags.String("graph", filepath.Join(".ua", "knowledge-graph.json"), "graph file")
	text := flags.String("text", "", "search text")
	limit := flags.Int("limit", 20, "maximum results")
	_ = flags.Parse(args)
	graph, err := understand.LoadGraph(*graphPath)
	if err != nil {
		return err
	}
	return writeJSON("", understand.Query(graph, *text, *limit))
}

func runDiff(args []string) error {
	flags := flag.NewFlagSet("diff", flag.ExitOnError)
	oldPath := flags.String("old", "", "old graph")
	newPath := flags.String("new", "", "new graph")
	out := flags.String("out", "", "write JSON here instead of stdout")
	_ = flags.Parse(args)
	if *oldPath == "" || *newPath == "" {
		return fmt.Errorf("diff requires --old and --new")
	}
	oldGraph, err := understand.LoadGraph(*oldPath)
	if err != nil {
		return err
	}
	newGraph, err := understand.LoadGraph(*newPath)
	if err != nil {
		return err
	}
	return writeJSON(*out, understand.Diff(oldGraph, newGraph))
}

func runExport(args []string) error {
	flags := flag.NewFlagSet("export", flag.ExitOnError)
	graphPath := flags.String("graph", filepath.Join(".ua", "knowledge-graph.json"), "graph file")
	format := flags.String("format", "json", "json, dot or mermaid")
	out := flags.String("out", "", "write here instead of stdout")
	_ = flags.Parse(args)
	graph, err := understand.LoadGraph(*graphPath)
	if err != nil {
		return err
	}
	switch *format {
	case "json":
		return writeJSON(*out, graph)
	case "dot":
		return writeText(*out, understand.ExportDOT(graph))
	case "mermaid":
		return writeText(*out, understand.ExportMermaid(graph))
	default:
		return fmt.Errorf("unknown format %q", *format)
	}
}

func runGoFrag(args []string) error {
	flags := flag.NewFlagSet("gofrag", flag.ExitOnError)
	root := flags.String("root", ".", "tree to scan")
	out := flags.String("out", "", "write JSON here instead of stdout")
	_ = flags.Parse(args)
	result, err := understand.Scan(*root, nil)
	if err != nil {
		return err
	}
	fragments, err := understand.GoFragments(*root, result.Files)
	if err != nil {
		return err
	}
	return writeJSON(*out, map[string]any{"fragments": fragments})
}

func runMCP(args []string) error {
	flags := flag.NewFlagSet("mcp", flag.ExitOnError)
	graphPath := flags.String("graph", filepath.Join(".ua", "knowledge-graph.json"), "graph file")
	_ = flags.Parse(args)
	return understand.ServeMCP(*graphPath, os.Stdin, os.Stdout)
}

func writeJSON(out string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeText(out, string(data)+"\n")
}

func writeText(out, text string) error {
	if out == "" {
		_, err := fmt.Fprint(os.Stdout, text)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, []byte(text), 0o644)
}
