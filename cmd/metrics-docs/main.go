// Package main generates the Markdown metrics reference used by the Zensical
// metrics endpoint page. It reads the Prometheus namespace and collector
// declarations, then writes each metric's name, type, help text, and labels as
// a table. The Zensical extension runs this command while rendering the page,
// keeping the reference in sync without publishing Go source or live samples.
package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// prometheusImportPath is the import path used by the source files to reference the Prometheus client.
const prometheusImportPath = "github.com/prometheus/client_golang/prometheus"

// metric holds the metadata extracted from one Prometheus collector declaration.
type metric struct {
	name   string
	kind   string
	help   string
	labels []string
}

// collectorKind describes the Prometheus constructor used to create a metric.
type collectorKind struct {
	metricType string
	options    string
	hasLabels  bool
}

// collectorKinds keeps supported constructors explicit so new collector forms fail instead of
// being silently omitted from the documentation.
var collectorKinds = map[string]collectorKind{
	"NewCounter":      {metricType: "Counter", options: "CounterOpts"},
	"NewCounterVec":   {metricType: "Counter", options: "CounterOpts", hasLabels: true},
	"NewGaugeVec":     {metricType: "Gauge", options: "GaugeOpts", hasLabels: true},
	"NewHistogramVec": {metricType: "Histogram", options: "HistogramOpts", hasLabels: true},
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)

		os.Exit(1)
	}
}

func run() error {
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	output, err := generate(root)
	if err != nil {
		return err
	}

	if _, err := io.WriteString(os.Stdout, output); err != nil {
		return fmt.Errorf("write generated metrics Markdown: %w", err)
	}

	return nil
}

// generate reads the namespace and collectors from their source files and
// returns the Markdown table used by Zensical.
func generate(root string) (string, error) {
	namespaceFile := filepath.Join(root, "internal", "prometheus", "metrics.go")

	namespaceSource, err := os.ReadFile(namespaceFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", namespaceFile, err)
	}

	namespace, err := parseNamespace(namespaceFile, namespaceSource)
	if err != nil {
		return "", err
	}

	collectorsFile := filepath.Join(root, "internal", "prometheus", "collectors.go")

	collectorsSource, err := os.ReadFile(collectorsFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", collectorsFile, err)
	}

	metrics, err := parseCollectors(collectorsFile, collectorsSource, namespace)
	if err != nil {
		return "", err
	}

	return renderMarkdown(metrics), nil
}

// parseNamespace resolves the shared namespace used to build full metric names.
func parseNamespace(filename string, source []byte) (string, error) {
	fileSet := token.NewFileSet()

	file, err := parser.ParseFile(fileSet, filename, source, parser.AllErrors)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", filename, err)
	}

	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}

		for _, specification := range general.Specs {
			values, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}

			for index, name := range values.Names {
				if name.Name != "MetricsNamespace" {
					continue
				}

				if index >= len(values.Values) {
					break
				}

				namespace, err := stringLiteral(values.Values[index])
				if err != nil {
					return "", fmt.Errorf("%s: MetricsNamespace: %w", filename, err)
				}

				return namespace, nil
			}
		}
	}

	return "", fmt.Errorf("%s: MetricsNamespace constant not found", filename)
}

// parseCollectors preserves declaration order and rejects unsupported
// prometheus.New* calls so the table cannot silently become incomplete.
func parseCollectors(filename string, source []byte, namespace string) ([]metric, error) {
	fileSet := token.NewFileSet()

	file, err := parser.ParseFile(fileSet, filename, source, parser.AllErrors)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}

	prometheusPackage, err := prometheusPackageName(file, filename)
	if err != nil {
		return nil, err
	}

	var metrics []metric

	seenNames := make(map[string]struct{})

	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.VAR {
			continue
		}

		for _, specification := range general.Specs {
			values, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}

			if len(values.Names) != len(values.Values) {
				for _, value := range values.Values {
					call, ok := value.(*ast.CallExpr)
					if ok && isPrometheusConstructor(call, prometheusPackage) {
						return nil, sourceError(fileSet, value.Pos(), "a collector declaration must have one variable per constructor")
					}
				}

				continue
			}

			for index, value := range values.Values {
				call, ok := value.(*ast.CallExpr)
				if !ok || !isPrometheusConstructor(call, prometheusPackage) {
					continue
				}

				selector := call.Fun.(*ast.SelectorExpr)

				kind, ok := collectorKinds[selector.Sel.Name]
				if !ok {
					return nil, sourceError(fileSet, call.Pos(), fmt.Sprintf("unsupported Prometheus collector constructor %q", selector.Sel.Name))
				}

				parsed, err := parseCollector(fileSet, call, prometheusPackage, namespace, values.Names[index].Name, kind)
				if err != nil {
					return nil, err
				}

				if _, found := seenNames[parsed.name]; found {
					return nil, sourceError(fileSet, call.Pos(), fmt.Sprintf("duplicate metric name %q", parsed.name))
				}

				seenNames[parsed.name] = struct{}{}
				metrics = append(metrics, parsed)
			}
		}
	}

	if len(metrics) == 0 {
		return nil, fmt.Errorf("%s: no Prometheus collectors found", filename)
	}

	return metrics, nil
}

// prometheusPackageName returns the package name used to import the Prometheus client.
func prometheusPackageName(file *ast.File, filename string) (string, error) {
	for _, imported := range file.Imports {
		importPath, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return "", fmt.Errorf("%s: invalid import path: %w", filename, err)
		}

		if importPath != prometheusImportPath {
			continue
		}

		if imported.Name != nil {
			return imported.Name.Name, nil
		}

		return "prometheus", nil
	}

	return "", fmt.Errorf("%s: Prometheus client import not found", filename)
}

// isPrometheusConstructor accounts for the package alias used by the source file.
func isPrometheusConstructor(call *ast.CallExpr, packageName string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !strings.HasPrefix(selector.Sel.Name, "New") {
		return false
	}

	qualifier, ok := selector.X.(*ast.Ident)

	return ok && qualifier.Name == packageName
}

// parseCollector extracts metadata from one constructor's literal options.
// Non-literal metadata is rejected rather than producing an incomplete table.
func parseCollector(
	fileSet *token.FileSet,
	call *ast.CallExpr,
	packageName string,
	namespace string,
	variableName string,
	kind collectorKind,
) (metric, error) {
	expectedArguments := 1
	if kind.hasLabels {
		expectedArguments++
	}

	if len(call.Args) != expectedArguments {
		return metric{}, sourceError(fileSet, call.Pos(), fmt.Sprintf("%s must have %d arguments", variableName, expectedArguments))
	}

	options, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		return metric{}, sourceError(fileSet, call.Args[0].Pos(), variableName+" options must be a struct literal")
	}

	optionsType, ok := options.Type.(*ast.SelectorExpr)
	if !ok {
		return metric{}, sourceError(fileSet, options.Pos(), variableName+" options type must be a Prometheus options type")
	}

	qualifier, ok := optionsType.X.(*ast.Ident)
	if !ok || qualifier.Name != packageName || optionsType.Sel.Name != kind.options {
		return metric{}, sourceError(fileSet, options.Pos(), fmt.Sprintf("%s must use %s.%s", variableName, packageName, kind.options))
	}

	metricName, found, err := stringField(options, "Name")
	if err != nil {
		return metric{}, sourceError(fileSet, options.Pos(), variableName+" Name: "+err.Error())
	}

	if !found || metricName == "" {
		return metric{}, sourceError(fileSet, options.Pos(), variableName+" has no metric Name")
	}

	help, found, err := stringField(options, "Help")
	if err != nil {
		return metric{}, sourceError(fileSet, options.Pos(), variableName+" Help: "+err.Error())
	}

	if !found {
		return metric{}, sourceError(fileSet, options.Pos(), variableName+" has no metric Help")
	}

	metricNamespace := ""

	namespaceExpression, found, err := fieldExpression(options, "Namespace")
	if err != nil {
		return metric{}, sourceError(fileSet, options.Pos(), variableName+" Namespace: "+err.Error())
	}

	if found {
		if identifier, ok := namespaceExpression.(*ast.Ident); ok {
			if identifier.Name != "MetricsNamespace" {
				return metric{}, sourceError(fileSet, namespaceExpression.Pos(), variableName+" uses an unsupported Namespace constant")
			}

			metricNamespace = namespace
		} else {
			metricNamespace, err = stringLiteral(namespaceExpression)
			if err != nil {
				return metric{}, sourceError(fileSet, namespaceExpression.Pos(), variableName+" Namespace: "+err.Error())
			}
		}
	}

	subsystem := ""

	subsystemExpression, found, err := fieldExpression(options, "Subsystem")
	if err != nil {
		return metric{}, sourceError(fileSet, options.Pos(), variableName+" Subsystem: "+err.Error())
	}

	if found {
		subsystem, err = stringLiteral(subsystemExpression)
		if err != nil {
			return metric{}, sourceError(fileSet, subsystemExpression.Pos(), variableName+" Subsystem: "+err.Error())
		}
	}

	fullName := strings.Join(nonEmptyStrings(metricNamespace, subsystem, metricName), "_")

	labels := []string(nil)
	if kind.hasLabels {
		labels, err = stringSlice(call.Args[1])
		if err != nil {
			return metric{}, sourceError(fileSet, call.Args[1].Pos(), variableName+" labels: "+err.Error())
		}
	}

	return metric{name: fullName, kind: kind.metricType, help: help, labels: labels}, nil
}

// fieldExpression finds a named option without depending on struct field order.
func fieldExpression(options *ast.CompositeLit, fieldName string) (ast.Expr, bool, error) {
	for _, element := range options.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return nil, false, errors.New("options must use named fields")
		}

		name, ok := field.Key.(*ast.Ident)
		if !ok {
			return nil, false, errors.New("options field name is not an identifier")
		}

		if name.Name == fieldName {
			return field.Value, true, nil
		}
	}

	return nil, false, nil
}

// stringField reads a quoted Go string from a named option.
func stringField(options *ast.CompositeLit, fieldName string) (string, bool, error) {
	expression, found, err := fieldExpression(options, fieldName)
	if err != nil || !found {
		return "", found, err
	}

	value, err := stringLiteral(expression)

	return value, true, err
}

// stringLiteral reads a quoted Go string from an AST expression.
func stringLiteral(expression ast.Expr) (string, error) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", errors.New("value must be a string literal")
	}

	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", fmt.Errorf("invalid string literal: %w", err)
	}

	return value, nil
}

// stringSlice reads label names from the []string literal used by vector metrics.
func stringSlice(expression ast.Expr) ([]string, error) {
	literal, ok := expression.(*ast.CompositeLit)
	if !ok {
		return nil, errors.New("value must be a []string literal")
	}

	arrayType, ok := literal.Type.(*ast.ArrayType)
	if !ok || arrayType.Len != nil {
		return nil, errors.New("value must be a []string literal")
	}

	elementType, ok := arrayType.Elt.(*ast.Ident)
	if !ok || elementType.Name != "string" {
		return nil, errors.New("value must be a []string literal")
	}

	labels := make([]string, 0, len(literal.Elts))
	for _, element := range literal.Elts {
		label, err := stringLiteral(element)
		if err != nil {
			return nil, err
		}

		labels = append(labels, label)
	}

	return labels, nil
}

// nonEmptyStrings returns a slice of the non-empty strings in the input.
func nonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}

	return result
}

// sourceError formats an error with the source file and line number.
func sourceError(fileSet *token.FileSet, position token.Pos, message string) error {
	return fmt.Errorf("%s: %s", fileSet.Position(position), message)
}

// renderMarkdown emits one table row per collector in declaration order.
func renderMarkdown(metrics []metric) string {
	var output strings.Builder
	output.WriteString("| Metric name | Type | Description | Labels |\n")
	output.WriteString("| --- | --- | --- | --- |\n")

	for _, item := range metrics {
		labels := "None"
		if len(item.labels) > 0 {
			formatted := make([]string, 0, len(item.labels))
			for _, label := range item.labels {
				formatted = append(formatted, "`"+label+"`")
			}

			labels = strings.Join(formatted, ", ")
		}

		_, _ = fmt.Fprintf(
			&output,
			"| `%s` | %s | %s | %s |\n",
			item.name,
			item.kind,
			tableCell(item.help),
			labels,
		)
	}

	return output.String()
}

// tableCell keeps help text inside one Markdown table cell.
func tableCell(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "|", "\\|")

	return strings.ReplaceAll(value, "\n", "<br>")
}
