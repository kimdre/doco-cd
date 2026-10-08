package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

type fieldDescriptions map[string]map[string]string

func readDescriptions(root string) (fieldDescriptions, error) {
	descriptions := make(fieldDescriptions)

	for _, directory := range []string{"internal/config/deploy", "internal/config", "internal/secretprovider/types"} {
		files, err := filepath.Glob(filepath.Join(root, directory, "*.go"))
		if err != nil {
			return nil, fmt.Errorf("find configuration source files: %w", err)
		}

		for _, filename := range files {
			if strings.HasSuffix(filename, "_test.go") {
				continue
			}

			file, err := parser.ParseFile(token.NewFileSet(), filename, nil, parser.ParseComments)
			if err != nil {
				return nil, fmt.Errorf("parse configuration descriptions: %w", err)
			}

			for _, declaration := range file.Decls {
				general, ok := declaration.(*ast.GenDecl)
				if !ok || general.Tok != token.TYPE {
					continue
				}

				for _, specification := range general.Specs {
					named, ok := specification.(*ast.TypeSpec)
					if !ok {
						continue
					}

					structure, ok := named.Type.(*ast.StructType)
					if !ok {
						continue
					}

					fields := make(map[string]string)

					for _, field := range structure.Fields.List {
						description := strings.TrimSpace(field.Doc.Text() + field.Comment.Text())

						for _, name := range field.Names {
							fields[name.Name] = description
						}
					}

					descriptions["github.com/kimdre/doco-cd/"+directory+"."+named.Name.Name] = fields
				}
			}
		}
	}

	return descriptions, nil
}

func readReconciliationEvents(root string) ([]string, error) {
	filename := filepath.Join(root, "internal/config/deploy/reconciliation.go")

	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse reconciliation events: %w", err)
	}

	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.VAR {
			continue
		}

		for _, specification := range general.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || value.Names[0].Name != "supportedReconciliationEvents" || len(value.Values) != 1 {
				continue
			}

			call, ok := value.Values[0].(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return nil, fmt.Errorf("%s: unsupported reconciliation event declaration", filename)
			}

			events := make([]string, 0, len(call.Args))

			for _, argument := range call.Args {
				literal, ok := argument.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return nil, fmt.Errorf("%s: reconciliation events must be string literals", filename)
				}

				event, err := strconv.Unquote(literal.Value)
				if err != nil {
					return nil, fmt.Errorf("read reconciliation event: %w", err)
				}

				events = append(events, event)
			}

			return events, nil
		}
	}

	return nil, fmt.Errorf("%s: supported reconciliation events not found", filename)
}
