package understand

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// GoFragments parses Go files with the standard library and emits the same
// fragment shape the Python core emits. This is the native-first path for the
// one language Go can parse without a C dependency; the Python seam remains the
// only merger, so no graph-building rule is duplicated.
func GoFragments(root string, records []FileRecord) ([]Fragment, error) {
	fragments := make([]Fragment, 0)
	for _, record := range records {
		if record.Language != "go" {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(record.Path)), nil, parser.ParseComments)
		if err != nil {
			fragments = append(fragments, Fragment{Nodes: []Node{fileNode(record, "unparsable Go file")}, Edges: []Edge{}})
			continue
		}
		fragments = append(fragments, goFragment(record, fset, file))
	}
	return fragments, nil
}

func fileNode(record FileRecord, summary string) Node {
	return Node{
		ID:         "file:" + record.Path,
		Type:       "file",
		Name:       path.Base(record.Path),
		FilePath:   record.Path,
		Summary:    summary,
		Tags:       []string{"go"},
		Complexity: "moderate",
	}
}

func goFragment(record FileRecord, fset *token.FileSet, file *ast.File) Fragment {
	fragment := Fragment{Nodes: []Node{fileNode(record, "Go file "+record.Path)}, Edges: []Edge{}}
	add := func(node Node) { fragment.Nodes = append(fragment.Nodes, node) }
	edge := func(source, target, kind string) {
		fragment.Edges = append(fragment.Edges, Edge{Source: source, Target: target, Type: kind, Direction: "forward", Weight: 0.5})
	}
	line := func(pos token.Pos) int { return fset.Position(pos).Line }

	packageID := "module:" + file.Name.Name
	add(Node{ID: packageID, Type: "module", Name: file.Name.Name, Summary: "Go package " + file.Name.Name, Tags: []string{"go", "package"}, Complexity: "moderate"})
	edge("file:"+record.Path, packageID, "contains")

	for _, spec := range file.Imports {
		name, err := strconv.Unquote(spec.Path.Value)
		if err != nil || name == "" {
			continue
		}
		id := "module:" + name
		add(Node{ID: id, Type: "module", Name: name, Summary: "Go import " + name, Tags: []string{"go", "import"}, Complexity: "moderate"})
		edge("file:"+record.Path, id, "imports")
	}

	typesByName := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			name := typeSpec.Name.Name
			id := fmt.Sprintf("class:%s:%s:%d", record.Path, name, line(typeSpec.Pos()))
			typesByName[name] = id
			kind := "Go type " + name
			switch typeSpec.Type.(type) {
			case *ast.StructType:
				kind = "Go struct " + name
			case *ast.InterfaceType:
				kind = "Go interface " + name
			}
			add(Node{ID: id, Type: "class", Name: name, FilePath: record.Path, Summary: kind, Tags: []string{"go", "type"}, Complexity: "moderate", LineRange: []int{line(typeSpec.Pos()), line(typeSpec.End())}})
			edge("file:"+record.Path, id, "contains")
		}
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			parent := typesByName[typeSpec.Name.Name]
			if structType, ok := typeSpec.Type.(*ast.StructType); ok && structType.Fields != nil {
				for _, field := range structType.Fields.List {
					if len(field.Names) != 0 {
						continue
					}
					embedded := strings.TrimPrefix(types.ExprString(field.Type), "*")
					embedded = embedded[strings.LastIndex(embedded, ".")+1:]
					if target, ok := typesByName[embedded]; ok {
						edge(parent, target, "inherits")
					}
				}
			}
		}
	}

	functions := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		parent := ""
		display := fn.Name.Name
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			receiver := strings.TrimPrefix(types.ExprString(fn.Recv.List[0].Type), "*")
			receiver = receiver[strings.LastIndex(receiver, ".")+1:]
			parent = typesByName[receiver]
			display = receiver + "." + fn.Name.Name
		}
		id := fmt.Sprintf("function:%s:%s:%d", record.Path, display, line(fn.Pos()))
		functions[fn.Name.Name] = id
		add(Node{ID: id, Type: "function", Name: display, FilePath: record.Path, Summary: "Go function " + display, Tags: []string{"go", "function"}, Complexity: goComplexity(fn.Body), LineRange: []int{line(fn.Pos()), line(fn.End())}})
		if parent != "" {
			edge(parent, id, "contains")
		} else {
			edge("file:"+record.Path, id, "contains")
		}
		if isHTTPHandler(fn) {
			endpointID := fmt.Sprintf("endpoint:%s:%s", record.Path, fn.Name.Name)
			add(Node{ID: endpointID, Type: "endpoint", Name: fn.Name.Name, FilePath: record.Path, Summary: "HTTP handler " + fn.Name.Name, Tags: []string{"go", "http"}, Complexity: "moderate"})
			edge(id, endpointID, "serves")
		}
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		caller := functions[fn.Name.Name]
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			}
			if target, ok := functions[name]; ok && target != caller {
				edge(caller, target, "calls")
			}
			return true
		})
	}

	sort.Slice(fragment.Nodes, func(i, j int) bool { return fragment.Nodes[i].ID < fragment.Nodes[j].ID })
	sort.Slice(fragment.Edges, func(i, j int) bool {
		if fragment.Edges[i].Source != fragment.Edges[j].Source {
			return fragment.Edges[i].Source < fragment.Edges[j].Source
		}
		if fragment.Edges[i].Target != fragment.Edges[j].Target {
			return fragment.Edges[i].Target < fragment.Edges[j].Target
		}
		return fragment.Edges[i].Type < fragment.Edges[j].Type
	})
	return fragment
}

func goComplexity(body *ast.BlockStmt) string {
	if body == nil {
		return "simple"
	}
	branches := 0
	ast.Inspect(body, func(node ast.Node) bool {
		switch node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			branches++
		}
		return true
	})
	if branches <= 2 {
		return "simple"
	}
	if branches <= 8 {
		return "moderate"
	}
	return "complex"
}

func isHTTPHandler(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, field := range fn.Type.Params.List {
		text := types.ExprString(field.Type)
		if strings.Contains(text, "http.ResponseWriter") || strings.Contains(text, "gin.Context") || strings.Contains(text, "echo.Context") {
			return true
		}
	}
	return strings.HasPrefix(fn.Name.Name, "Handle")
}
