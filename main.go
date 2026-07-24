// fix-api-spec reads server.gen.go to extract enum type names and their constant names,
// then updates api-spec.yaml to ensure all enums are named schemas with x-enum-varnames.
//
// This prevents oapi-codegen from renaming enum constants when unrelated enums are
// added or removed from the spec.
//
// Usage: fix-api-spec -gen <server.gen.go> -spec <api-spec.yaml> [-out <output.yaml>]
//
// If -out is not specified, the spec file is modified in place.
// The tool is idempotent — running it multiple times produces the same result.
// If adding a new named schema would collide with an existing inline property enum,
// the inline enum is replaced with a $ref to the new standalone schema.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type enumType struct {
	name   string
	consts []enumConst
}

type enumConst struct {
	name  string
	value string
}

func main() {
	genFile := flag.String("gen", "", "path to server.gen.go")
	specFile := flag.String("spec", "", "path to api-spec.yaml")
	outFile := flag.String("out", "", "output file (defaults to -spec, modifying in place)")
	checkOnly := flag.Bool("check", false, "check mode: don't write output; exit non-zero if the spec would change OR if any generated constants need renaming")
	flag.Parse()

	if *genFile == "" || *specFile == "" {
		fmt.Fprintf(os.Stderr, "Usage: fix-api-spec -gen <server.gen.go> -spec <api-spec.yaml> [-out <output.yaml>] [-check]\n")
		os.Exit(1)
	}

	if *outFile == "" {
		*outFile = *specFile
	}

	// Step 1: Extract all enum types from server.gen.go using Go parser
	enums := extractEnums(*genFile)
	fmt.Fprintf(os.Stderr, "Found %d enum types in %s\n", len(enums), *genFile)

	// Step 2: Parse the OpenAPI spec
	specData, err := os.ReadFile(*specFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", *specFile, err)
		os.Exit(1)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(specData, &doc); err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", *specFile, err)
		os.Exit(1)
	}

	// Step 3: Find the schemas and parameters map nodes
	schemasNode := findSchemasNode(&doc)
	if schemasNode == nil {
		fmt.Fprintf(os.Stderr, "ERROR: could not find components/schemas in spec\n")
		os.Exit(1)
	}

	parametersNode := findParametersNode(&doc)

	// Step 4: Build set of existing named types (schemas + parameters)
	existingSchemas := make(map[string]bool)
	for i := 0; i < len(schemasNode.Content); i += 2 {
		existingSchemas[schemasNode.Content[i].Value] = true
	}

	// Named parameters also generate types (e.g. SortDirectionParam)
	existingParams := make(map[string]bool)
	if parametersNode != nil {
		for i := 0; i < len(parametersNode.Content); i += 2 {
			existingParams[parametersNode.Content[i].Value] = true
		}
	}

	// Find all type names generated from $ref parameter usages in path operations.
	// When a path uses $ref to a named parameter with an enum, the generator creates
	// a per-endpoint type like GetAnalyticsPerformanceParamsSortDirection.
	// These can't be controlled via schemas — they need x-enum-varnames on the parameter.
	paramRefTypes, _ := findParamRefTypes(&doc, parametersNode)
	fmt.Fprintf(os.Stderr, "Found %d parameter-ref generated types\n", len(paramRefTypes))

	// Step 5: Find ALL inline enum nodes anywhere in the YAML tree.
	// This covers schema properties, path parameters, request bodies, response bodies, etc.
	allInlineEnumNodes := findAllInlineEnumNodes(&doc, schemasNode)
	fmt.Fprintf(os.Stderr, "Found %d inline enum nodes in spec\n", len(allInlineEnumNodes))

	// Step 6: Classify and process each enum type
	var existingCount, extractedCount int
	var renames []string

	for _, e := range enums {
		// Verify all const names are fully qualified (prefixed with type name).
		// Short names (e.g. "Daily" instead of "TypeNameDaily") are unstable —
		// replace them with the stable form.
		for i, c := range e.consts {
			if !strings.HasPrefix(c.name, e.name) {
				stableName := e.name + snakeToPascal(c.value)
				renames = append(renames, fmt.Sprintf("%s -> %s", c.name, stableName))
				e.consts[i].name = stableName
			}
		}

		if existingSchemas[e.name] {
			addVarnamesToSchema(schemasNode, e)
			existingCount++

			if node, ok := allInlineEnumNodes[e.name]; ok {
				replaceEnumNodeWithRef(node, e.name)
				extractedCount++
			}
		} else if existingParams[e.name] {
			extractParamEnumToSchema(schemasNode, parametersNode, e)
			extractedCount++
		} else if paramRefTypes[e.name] {
			// Per-endpoint type eliminated by shared schema — renames already recorded above.
			continue
		} else if node, ok := allInlineEnumNodes[e.name]; ok {
			addEnumSchema(schemasNode, e)
			replaceEnumNodeWithRef(node, e.name)
			extractedCount++
		} else {
			addEnumSchema(schemasNode, e)
			extractedCount++
		}
	}

	fmt.Fprintf(os.Stderr, "  %d existing schemas updated with x-enum-varnames\n", existingCount)
	fmt.Fprintf(os.Stderr, "  %d enum schemas extracted/added\n", extractedCount)
	if len(renames) > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: %d enum constants will be renamed:\n", len(renames))
		for _, r := range renames {
			fmt.Fprintf(os.Stderr, "    %s\n", r)
		}
	}

	// Step 7: Marshal the (possibly modified) doc back to YAML.
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	enc.Close()
	out := []byte(buf.String())

	// Check mode: don't write anything. Exit non-zero if the spec
	// content would change OR if any generated constants need
	// renaming. Both conditions mean the spec is not in a fully
	// canonicalized state — the calling build should surface that
	// as a build failure so it gets fixed before downstream
	// consumers regenerate.
	if *checkOnly {
		dirty := false
		if !bytes.Equal(specData, out) {
			fmt.Fprintf(os.Stderr, "FAIL: %s would change if -check were dropped\n", *specFile)
			dirty = true
		}
		if len(renames) > 0 {
			fmt.Fprintf(os.Stderr, "FAIL: %d generated constants need renaming (re-run without -check to fix the spec)\n", len(renames))
			dirty = true
		}
		if dirty {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "OK: %s is canonical\n", *specFile)
		return
	}

	if err := os.WriteFile(*outFile, out, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *outFile, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Updated %s\n", *outFile)
}

// extractEnums uses the Go AST parser to extract enum types from server.gen.go.
func extractEnums(filename string) []enumType {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", filename, err)
		os.Exit(1)
	}

	// First pass: find all type definitions that are string types (enum type aliases)
	stringTypes := make(map[string]bool)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts := spec.(*ast.TypeSpec)
			if ident, ok := ts.Type.(*ast.Ident); ok && ident.Name == "string" {
				stringTypes[ts.Name.Name] = true
			}
		}
	}

	// Second pass: find const blocks and group by type
	enumMap := make(map[string][]enumConst)
	var enumOrder []string

	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}

			// Get the type name
			var typeName string
			if vs.Type != nil {
				if ident, ok := vs.Type.(*ast.Ident); ok {
					typeName = ident.Name
				}
			}
			if typeName == "" || !stringTypes[typeName] {
				continue
			}

			// Get the string value
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}

			if _, exists := enumMap[typeName]; !exists {
				enumOrder = append(enumOrder, typeName)
			}
			enumMap[typeName] = append(enumMap[typeName], enumConst{
				name:  vs.Names[0].Name,
				value: value,
			})
		}
	}

	var result []enumType
	for _, name := range enumOrder {
		result = append(result, enumType{name: name, consts: enumMap[name]})
	}
	return result
}

// inlineEnumNode represents a mapping node in the YAML tree that contains
// enum: [...]. The node's content can be replaced with a $ref.
type inlineEnumNode struct {
	node     *yaml.Node // the mapping node containing enum
	typeName string     // the type name the generator would produce for this node
}

// findAllInlineEnumNodes walks the entire YAML tree and finds all unnamed enum nodes.
// An enum is "named" if it's a direct child of components/schemas. Everything else is inline.
// Returns a map of generated type name -> node.
func findAllInlineEnumNodes(doc *yaml.Node, schemasNode *yaml.Node) map[string]*inlineEnumNode {
	// Build a set of named enum schema nodes to skip.
	namedEnumNodes := make(map[*yaml.Node]bool)
	if schemasNode != nil {
		for i := 0; i < len(schemasNode.Content)-1; i += 2 {
			valueNode := schemasNode.Content[i+1]
			if findMapValue(valueNode, "enum") != nil {
				namedEnumNodes[valueNode] = true
			}
		}
	}

	result := make(map[string]*inlineEnumNode)

	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}

	// Walk components/schemas
	if schemasNode != nil {
		for i := 0; i < len(schemasNode.Content)-1; i += 2 {
			schemaName := schemasNode.Content[i].Value
			schemaNode := schemasNode.Content[i+1]
			if namedEnumNodes[schemaNode] {
				continue
			}
			walkEnums(schemaNode, schemaName, namedEnumNodes, result)
		}
	}

	// Walk paths
	pathsNode := findMapValue(root, "paths")
	if pathsNode != nil {
		for i := 0; i < len(pathsNode.Content)-1; i += 2 {
			pathStr := pathsNode.Content[i].Value
			pathNode := pathsNode.Content[i+1]

			for j := 0; j < len(pathNode.Content)-1; j += 2 {
				method := pathNode.Content[j].Value
				methodNode := pathNode.Content[j+1]

				opID := findMapValue(methodNode, "operationId")
				var opPrefix string
				if opID != nil {
					opPrefix = opID.Value
				} else {
					opPrefix = buildOperationID(method, pathStr)
				}

				// Parameters
				paramsNode := findMapValue(methodNode, "parameters")
				if paramsNode != nil {
					for _, paramNode := range paramsNode.Content {
						if findMapValue(paramNode, "$ref") != nil {
							continue
						}
						nameNode := findMapValue(paramNode, "name")
						if nameNode == nil {
							continue
						}
						schemaNode := findMapValue(paramNode, "schema")
						if schemaNode == nil {
							continue
						}
						paramTypeName := opPrefix + "Params" + snakeToPascal(nameNode.Value)
						walkEnums(schemaNode, paramTypeName, namedEnumNodes, result)
					}
				}

				// Request body
				reqBody := findMapValue(methodNode, "requestBody")
				if reqBody != nil {
					content := findMapValue(reqBody, "content")
					if content != nil {
						appJSON := findMapValue(content, "application/json")
						if appJSON != nil {
							schema := findMapValue(appJSON, "schema")
							if schema != nil {
								walkEnums(schema, opPrefix+"JSONBody", namedEnumNodes, result)
							}
						}
					}
				}
			}
		}
	}

	return result
}

// walkEnums recursively finds all inline enum nodes starting from a given node.
// It handles direct enums, properties, nested objects, and array items.
func walkEnums(node *yaml.Node, typeName string, skip map[*yaml.Node]bool, result map[string]*inlineEnumNode) {
	if node == nil || skip[node] {
		return
	}

	// If this node itself has an enum, record it
	if findMapValue(node, "enum") != nil {
		result[typeName] = &inlineEnumNode{node: node, typeName: typeName}
	}

	// Recurse into properties
	props := findMapValue(node, "properties")
	if props != nil {
		for i := 0; i < len(props.Content)-1; i += 2 {
			propName := props.Content[i].Value
			propNode := props.Content[i+1]
			walkEnums(propNode, typeName+snakeToPascal(propName), skip, result)
		}
	}

	// Recurse into array items
	items := findMapValue(node, "items")
	if items != nil {
		walkEnums(items, typeName, skip, result)
	}
}

// replaceEnumNodeWithRef replaces a mapping node's content with a $ref.
func replaceEnumNodeWithRef(node *inlineEnumNode, schemaName string) {
	node.node.Content = []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "$ref"},
		{Kind: yaml.ScalarNode, Value: fmt.Sprintf("#/components/schemas/%s", schemaName)},
	}
}

// findSchemasNode navigates the YAML tree to find the components/schemas mapping node.
func findSchemasNode(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}

	components := findMapValue(doc, "components")
	if components == nil {
		return nil
	}
	return findMapValue(components, "schemas")
}

// findParametersNode navigates the YAML tree to find the components/parameters mapping node.
func findParametersNode(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}

	components := findMapValue(doc, "components")
	if components == nil {
		return nil
	}
	return findMapValue(components, "parameters")
}



// extractParamEnumToSchema extracts a named parameter's inline enum schema into
// a standalone named schema, and replaces the parameter's schema with a $ref.
// This eliminates per-endpoint types in favor of one shared type.
func extractParamEnumToSchema(schemas *yaml.Node, params *yaml.Node, e enumType) {
	paramNode := findMapValue(params, e.name)
	if paramNode == nil {
		fmt.Fprintf(os.Stderr, "  WARNING: parameter %s not found\n", e.name)
		return
	}

	schemaNode := findMapValue(paramNode, "schema")
	if schemaNode == nil {
		fmt.Fprintf(os.Stderr, "  WARNING: parameter %s has no schema\n", e.name)
		return
	}

	// Add the enum as a named schema with x-enum-varnames
	addEnumSchema(schemas, e)

	// Replace the parameter's inline schema with a $ref
	schemaNode.Kind = yaml.MappingNode
	schemaNode.Content = []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "$ref"},
		{Kind: yaml.ScalarNode, Value: fmt.Sprintf("#/components/schemas/%s", e.name)},
	}
}

// findParamRefTypes scans all path operations for $ref parameter usages and computes
// the type names that oapi-codegen generates for each.
// For example, if /analytics/performance uses $ref: '#/components/parameters/SortDirectionParam',
// and SortDirectionParam has schema field "sort_direction" with an enum, the generator creates
// type GetAnalyticsPerformanceParamsSortDirection.
func findParamRefTypes(doc *yaml.Node, parametersNode *yaml.Node) (map[string]bool, map[string]string) {
	result := make(map[string]bool)
	source := make(map[string]string) // per-endpoint type -> parameter name
	if parametersNode == nil {
		return result, source
	}

	// Build map of parameter name -> schema field name (the "name" field in the parameter)
	paramFieldNames := make(map[string]string)
	paramHasEnum := make(map[string]bool)
	for i := 0; i < len(parametersNode.Content)-1; i += 2 {
		paramName := parametersNode.Content[i].Value
		paramNode := parametersNode.Content[i+1]

		nameNode := findMapValue(paramNode, "name")
		if nameNode != nil {
			paramFieldNames[paramName] = nameNode.Value
		}

		schemaNode := findMapValue(paramNode, "schema")
		if schemaNode != nil && findMapValue(schemaNode, "enum") != nil {
			paramHasEnum[paramName] = true
		}
	}

	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}

	pathsNode := findMapValue(root, "paths")
	if pathsNode == nil {
		return result, source
	}

	// Iterate paths
	for i := 0; i < len(pathsNode.Content)-1; i += 2 {
		pathStr := pathsNode.Content[i].Value
		pathNode := pathsNode.Content[i+1]

		// Iterate methods (get, post, etc.)
		for j := 0; j < len(pathNode.Content)-1; j += 2 {
			method := pathNode.Content[j].Value
			methodNode := pathNode.Content[j+1]

			paramsNode := findMapValue(methodNode, "parameters")
			if paramsNode == nil {
				continue
			}

			// Compute the operation ID prefix that oapi-codegen uses
			opID := findMapValue(methodNode, "operationId")
			var opPrefix string
			if opID != nil {
				opPrefix = opID.Value
			} else {
				opPrefix = buildOperationID(method, pathStr)
			}

			// Check each parameter for $ref
			for _, paramNode := range paramsNode.Content {
				refNode := findMapValue(paramNode, "$ref")
				if refNode == nil {
					continue
				}

				// Extract parameter name from $ref: '#/components/parameters/ParamName'
				ref := refNode.Value
				prefix := "#/components/parameters/"
				if !strings.HasPrefix(ref, prefix) {
					continue
				}
				paramName := ref[len(prefix):]

				if !paramHasEnum[paramName] {
					continue
				}

				fieldName, ok := paramFieldNames[paramName]
				if !ok {
					continue
				}

				// The generated type name is: OperationIDParams + PascalCase(fieldName)
				typeName := opPrefix + "Params" + snakeToPascal(fieldName)
				result[typeName] = true
				source[typeName] = paramName
			}
		}
	}

	return result, source
}

// buildOperationID builds the operation ID that oapi-codegen generates from method + path.
// e.g., GET /analytics/performance -> GetAnalyticsPerformance
func buildOperationID(method, path string) string {
	// Capitalize method
	op := strings.ToUpper(method[:1]) + method[1:]

	// Split path and capitalize each segment
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for _, p := range parts {
		if strings.HasPrefix(p, "{") {
			// Path parameter: {transactionId} -> TransactionId
			p = strings.Trim(p, "{}")
		}
		op += snakeToPascal(p)
	}

	return op
}

// addVarnamesToParameter adds x-enum-varnames to a named parameter's inline schema enum.

// addVarnamesToParameter adds x-enum-varnames to a named parameter's inline schema enum.

// findMapValue finds the value node for a given key in a mapping node.
func findMapValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// snakeToPascal converts a snake_case or kebab-case string to PascalCase.
func snakeToPascal(s string) string {
	// Replace hyphens with underscores so both are treated as word separators
	s = strings.ReplaceAll(s, "-", "_")
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, p := range parts {
		if len(p) > 0 {
			b.WriteString(strings.ToUpper(p[:1]))
			b.WriteString(p[1:])
		}
	}
	return b.String()
}

// addVarnamesToSchema adds or updates x-enum-varnames on an existing schema.
// The varnames must be ordered to match the schema's enum values.
func addVarnamesToSchema(schemas *yaml.Node, e enumType) {
	schemaNode := findMapValue(schemas, e.name)
	if schemaNode == nil {
		return
	}

	varnames := buildOrderedVarnamesNode(schemaNode, e)

	// Check if x-enum-varnames already exists
	for i := 0; i < len(schemaNode.Content)-1; i += 2 {
		if schemaNode.Content[i].Value == "x-enum-varnames" {
			schemaNode.Content[i+1] = varnames
			return
		}
	}

	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: "x-enum-varnames"}
	schemaNode.Content = append(schemaNode.Content, keyNode, varnames)
}

// buildOrderedVarnamesNode builds a varnames sequence ordered to match the enum values.
// It looks for an "enum" key in the given node and orders varnames accordingly.
func buildOrderedVarnamesNode(node *yaml.Node, e enumType) *yaml.Node {
	// Build a map from value -> const name
	valueToName := make(map[string]string)
	for _, c := range e.consts {
		valueToName[c.value] = c.name
	}

	// Get the enum values in their declared order
	enumNode := findMapValue(node, "enum")
	if enumNode == nil {
		return buildVarnamesNode(e)
	}

	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, v := range enumNode.Content {
		name, ok := valueToName[v.Value]
		if !ok {
			fmt.Fprintf(os.Stderr, "  WARNING: enum value %q in %s not found in generated constants\n", v.Value, e.name)
			name = v.Value
		}
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: name})
	}
	return seq
}

// addEnumSchema adds a new enum schema to the schemas map.
func addEnumSchema(schemas *yaml.Node, e enumType) {
	// Build the schema node
	schemaNode := &yaml.Node{Kind: yaml.MappingNode}

	// type: string
	schemaNode.Content = append(schemaNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "type"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: "string"},
	)

	// enum: [values...]
	enumSeq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, c := range e.consts {
		enumSeq.Content = append(enumSeq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: c.value})
	}
	schemaNode.Content = append(schemaNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "enum"},
		enumSeq,
	)

	// x-enum-varnames: [names...]
	varnames := buildVarnamesNode(e)
	schemaNode.Content = append(schemaNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "x-enum-varnames"},
		varnames,
	)

	// Insert into schemas map (sorted position)
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: e.name}

	// Find insertion point to maintain alphabetical order
	insertIdx := len(schemas.Content)
	for i := 0; i < len(schemas.Content)-1; i += 2 {
		if schemas.Content[i].Value > e.name {
			insertIdx = i
			break
		}
	}

	// Insert key and value at position
	newContent := make([]*yaml.Node, 0, len(schemas.Content)+2)
	newContent = append(newContent, schemas.Content[:insertIdx]...)
	newContent = append(newContent, keyNode, schemaNode)
	newContent = append(newContent, schemas.Content[insertIdx:]...)
	schemas.Content = newContent
}

// buildVarnamesNode builds a YAML sequence node for x-enum-varnames.
func buildVarnamesNode(e enumType) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	for _, c := range e.consts {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: c.name})
	}
	return seq
}

