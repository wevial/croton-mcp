package bridge_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIMAPDependencyAndMutationMethodsStayInsideFacade(t *testing.T) {
	t.Parallel()

	_, testPath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate source file")
	}
	bridgeDirectory := filepath.Dir(testPath)
	files, err := filepath.Glob(filepath.Join(bridgeDirectory, "*.go"))
	if err != nil {
		t.Fatalf("list bridge sources: %v", err)
	}

	for _, sourcePath := range files {
		if strings.HasSuffix(sourcePath, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), sourcePath, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", sourcePath, err)
		}
		for _, imported := range parsed.Imports {
			importPath := strings.Trim(imported.Path.Value, "\"")
			if strings.HasPrefix(importPath, "github.com/emersion/go-imap/") && filepath.Base(sourcePath) != "imapclient.go" {
				t.Fatalf("go-imap import escaped facade: %s imports %s", sourcePath, imported.Path.Value)
			}
		}
		if filepath.Base(sourcePath) == "imapclient.go" {
			allowedClientMethods := map[string]bool{
				"WaitGreeting": true,
				"Caps":         true,
				"Close":        true,
				"Login":        true,
				"Noop":         true,
				"List":         true,
				"Status":       true,
				"Select":       true,
				"UIDSearch":    true,
				"Fetch":        true,
				"Logout":       true,
			}
			ast.Inspect(parsed, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !isIMAPClientReceiver(selector.X) {
					return true
				}
				if !allowedClientMethods[selector.Sel.Name] {
					t.Fatalf("go-imap client method outside exact facade allowlist: %s", selector.Sel.Name)
				}
				return true
			})
		}
		if parsed.Name.Name == "bridge" && filepath.Base(sourcePath) != "imapclient.go" {
			ast.Inspect(parsed, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch selector.Sel.Name {
				case "Append", "Copy", "Move", "Create", "Delete", "Rename", "Store", "Expunge", "UIDExpunge", "Subscribe", "Unsubscribe", "SetACL", "SetMetadata", "SetQuota", "UnselectAndExpunge", "Select":
					t.Fatalf("mutation-capable selector escaped facade: %s in %s", selector.Sel.Name, sourcePath)
				}
				return true
			})
		}
	}
}

func isIMAPClientReceiver(expression ast.Expr) bool {
	switch receiver := expression.(type) {
	case *ast.Ident:
		return receiver.Name == "client"
	case *ast.SelectorExpr:
		return receiver.Sel.Name == "client"
	default:
		return false
	}
}
