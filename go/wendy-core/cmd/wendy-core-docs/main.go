// Command wendy-core-docs extracts and compares the documented Wendy Core contract.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

type manifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	Version       string          `json:"version"`
	Contract      contractSources `json:"contract"`
	C             cAPI            `json:"c"`
	Protobuf      protobufAPI     `json:"protobuf"`
}

type contractSources struct {
	Ownership   string `json:"ownership"`
	Concurrency string `json:"concurrency"`
	Streaming   string `json:"streaming"`
	Errors      string `json:"errors"`
	Buffers     string `json:"buffers"`
	Patterns    string `json:"patterns"`
}

type cAPI struct {
	Functions []cFunction `json:"functions"`
}

type cFunction struct {
	Name         string       `json:"name"`
	Signature    string       `json:"signature"`
	Parameters   []cParameter `json:"parameters,omitempty"`
	Pattern      string       `json:"pattern,omitempty"`
	Ownership    string       `json:"ownership,omitempty"`
	Threading    string       `json:"threading,omitempty"`
	Cancellation string       `json:"cancellation,omitempty"`
	Availability string       `json:"availability,omitempty"`
	Comment      string       `json:"comment,omitempty"`
}

type cParameter struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type protobufAPI struct {
	Messages []protobufMessage `json:"messages"`
	Enums    []protobufEnum    `json:"enums,omitempty"`
	Services []protobufService `json:"services"`
}

type protobufMessage struct {
	Name    string          `json:"name"`
	Comment string          `json:"comment,omitempty"`
	Fields  []protobufField `json:"fields"`
}

type protobufField struct {
	Number   int32  `json:"number"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

type protobufEnum struct {
	Name    string              `json:"name"`
	Comment string              `json:"comment,omitempty"`
	Values  []protobufEnumValue `json:"values"`
}

type protobufEnumValue struct {
	Number  int32  `json:"number"`
	Name    string `json:"name"`
	Comment string `json:"comment,omitempty"`
}

type protobufService struct {
	Name    string           `json:"name"`
	Comment string           `json:"comment,omitempty"`
	Methods []protobufMethod `json:"methods"`
}

type protobufMethod struct {
	Name            string `json:"name"`
	Input           string `json:"input"`
	Output          string `json:"output"`
	CFunction       string `json:"cFunction,omitempty"`
	Pattern         string `json:"pattern,omitempty"`
	ServerStreaming bool   `json:"serverStreaming,omitempty"`
	ClientStreaming bool   `json:"clientStreaming,omitempty"`
	Comment         string `json:"comment,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	var err error
	switch os.Args[1] {
	case "snapshot":
		err = snapshot(os.Args[2:])
	case "compare":
		err = compare(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wendy-core-docs:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: wendy-core-docs <snapshot|compare> [options]")
	os.Exit(2)
}

func snapshot(arguments []string) error {
	flags := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	headers := flags.String("headers", "include/WendyCore", "public header directory")
	descriptor := flags.String("descriptor", "", "protobuf descriptor set")
	version := flags.String("version", "main", "API version label")
	output := flags.String("output", "api.json", "output manifest")
	markdown := flags.String("markdown", "", "optional protobuf reference Markdown")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *descriptor == "" {
		return errors.New("--descriptor is required")
	}

	functions, err := extractC(*headers)
	if err != nil {
		return err
	}
	protobuf, err := extractProtobuf(*descriptor)
	if err != nil {
		return err
	}
	result := manifest{
		SchemaVersion: 1,
		Version:       *version,
		Contract: contractSources{
			Ownership:   "documentation/c-contract/Ownership.md",
			Concurrency: "documentation/c-contract/Concurrency.md",
			Streaming:   "documentation/c-contract/Streaming.md",
			Errors:      "documentation/c-contract/Errors.md",
			Buffers:     "documentation/c-contract/Buffers.md",
			Patterns:    "documentation/binding-patterns",
		},
		C: cAPI{Functions: functions}, Protobuf: protobuf,
	}
	if err := writeJSON(*output, result); err != nil {
		return err
	}
	if *markdown != "" {
		if err := writeMarkdown(*markdown, result.Protobuf); err != nil {
			return err
		}
	}
	return nil
}

func compare(arguments []string) error {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	fromPath := flags.String("from", "", "older API manifest")
	toPath := flags.String("to", "", "newer API manifest")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *fromPath == "" || *toPath == "" {
		return errors.New("--from and --to are required")
	}
	from, err := readManifest(*fromPath)
	if err != nil {
		return err
	}
	to, err := readManifest(*toPath)
	if err != nil {
		return err
	}
	fmt.Printf("# Wendy Core %s → %s\n\n", from.Version, to.Version)
	changes := 0
	changes += compareFunctions(from.C.Functions, to.C.Functions)
	changes += compareMessages(from.Protobuf.Messages, to.Protobuf.Messages)
	changes += compareServices(from.Protobuf.Services, to.Protobuf.Services)
	if changes == 0 {
		fmt.Println("No API changes.")
	}
	return nil
}

func extractC(headerDirectory string) ([]cFunction, error) {
	umbrella := filepath.Join(headerDirectory, "WendyCore.h")
	includeRoot := filepath.Dir(headerDirectory)
	command := exec.Command(
		"clang", "-x", "c", "-std=c11", "-fsyntax-only", "-fparse-all-comments",
		"-I"+includeRoot, "-Xclang", "-ast-dump=json", umbrella,
	)
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("extracting C declarations: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("extracting C declarations: %w", err)
	}
	var root astNode
	if err := json.Unmarshal(output, &root); err != nil {
		return nil, fmt.Errorf("decoding clang AST: %w", err)
	}
	var functions []cFunction
	walkAST(root, func(node astNode) {
		if node.Kind != "FunctionDecl" || !strings.HasPrefix(node.Name, "Wendy") {
			return
		}
		function := cFunction{Name: node.Name, Signature: node.Type.QualType}
		for _, child := range node.Inner {
			switch child.Kind {
			case "ParmVarDecl":
				function.Parameters = append(function.Parameters, cParameter{Name: child.Name, Type: child.Type.QualType})
			case "FullComment":
				function.Comment = commentText(child)
			}
		}
		function.Pattern = commentLabel(function.Comment, "Binding pattern")
		function.Ownership = commentLabel(function.Comment, "Ownership")
		function.Threading = commentLabel(function.Comment, "Threading")
		function.Cancellation = commentLabel(function.Comment, "Cancellation")
		function.Availability = commentLabel(function.Comment, "Availability")
		functions = append(functions, function)
	})
	sort.Slice(functions, func(i, j int) bool { return functions[i].Name < functions[j].Name })
	return functions, nil
}

type astNode struct {
	Kind  string    `json:"kind"`
	Name  string    `json:"name"`
	Text  string    `json:"text"`
	Type  astType   `json:"type"`
	Inner []astNode `json:"inner"`
}

type astType struct {
	QualType string `json:"qualType"`
}

func walkAST(node astNode, visit func(astNode)) {
	visit(node)
	for _, child := range node.Inner {
		walkAST(child, visit)
	}
}

func commentText(node astNode) string {
	var pieces []string
	walkAST(node, func(child astNode) {
		if child.Kind == "TextComment" {
			text := strings.TrimSpace(child.Text)
			if text != "" {
				pieces = append(pieces, text)
			}
		}
	})
	return strings.Join(pieces, " ")
}

func commentLabel(comment, label string) string {
	marker := label + ": "
	start := strings.Index(comment, marker)
	if start < 0 {
		return ""
	}
	value := comment[start+len(marker):]
	for _, next := range []string{"Binding pattern", "Ownership", "Threading", "Cancellation", "Availability", "C bridge"} {
		if next == label {
			continue
		}
		if end := strings.Index(value, " "+next+": "); end >= 0 {
			value = value[:end]
		}
	}
	return strings.TrimSuffix(strings.TrimSpace(value), ".")
}

func extractProtobuf(path string) (protobufAPI, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return protobufAPI{}, err
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		return protobufAPI{}, fmt.Errorf("decoding descriptor set: %w", err)
	}
	var result protobufAPI
	for _, file := range set.File {
		if !strings.HasPrefix(file.GetPackage(), "wendy.core.") {
			continue
		}
		comments := sourceComments(file)
		for i, message := range file.MessageType {
			messagePath := []int32{4, int32(i)}
			entry := protobufMessage{Name: qualified(file.GetPackage(), message.GetName()), Comment: comments[key(messagePath)], Fields: []protobufField{}}
			for j, field := range message.Field {
				fieldPath := appendCopy(messagePath, 2, int32(j))
				entry.Fields = append(entry.Fields, protobufField{
					Number: field.GetNumber(), Name: field.GetJsonName(), Type: protobufType(field),
					Optional: field.GetProto3Optional(), Comment: comments[key(fieldPath)],
				})
			}
			result.Messages = append(result.Messages, entry)
			for j, enum := range message.EnumType {
				enumPath := appendCopy(messagePath, 4, int32(j))
				result.Enums = append(result.Enums, makeEnum(file.GetPackage()+"."+message.GetName(), enum, enumPath, comments))
			}
		}
		for i, enum := range file.EnumType {
			result.Enums = append(result.Enums, makeEnum(file.GetPackage(), enum, []int32{5, int32(i)}, comments))
		}
		for i, service := range file.Service {
			servicePath := []int32{6, int32(i)}
			entry := protobufService{Name: qualified(file.GetPackage(), service.GetName()), Comment: comments[key(servicePath)]}
			for j, method := range service.Method {
				methodPath := appendCopy(servicePath, 2, int32(j))
				comment := comments[key(methodPath)]
				entry.Methods = append(entry.Methods, protobufMethod{
					Name: method.GetName(), Input: strings.TrimPrefix(method.GetInputType(), "."),
					Output: strings.TrimPrefix(method.GetOutputType(), "."), CFunction: commentLabel(comment, "C bridge"),
					Pattern:         commentLabel(comment, "Binding pattern"),
					ServerStreaming: method.GetServerStreaming(), ClientStreaming: method.GetClientStreaming(),
					Comment: comment,
				})
			}
			result.Services = append(result.Services, entry)
		}
	}
	sort.Slice(result.Messages, func(i, j int) bool { return result.Messages[i].Name < result.Messages[j].Name })
	sort.Slice(result.Enums, func(i, j int) bool { return result.Enums[i].Name < result.Enums[j].Name })
	sort.Slice(result.Services, func(i, j int) bool { return result.Services[i].Name < result.Services[j].Name })
	return result, nil
}

func sourceComments(file *descriptorpb.FileDescriptorProto) map[string]string {
	result := make(map[string]string)
	for _, location := range file.GetSourceCodeInfo().GetLocation() {
		comment := strings.TrimSpace(location.GetLeadingComments())
		if comment == "" {
			comment = strings.TrimSpace(location.GetTrailingComments())
		}
		if comment != "" {
			result[key(location.Path)] = strings.Join(strings.Fields(comment), " ")
		}
	}
	return result
}

func makeEnum(prefix string, enum *descriptorpb.EnumDescriptorProto, path []int32, comments map[string]string) protobufEnum {
	entry := protobufEnum{Name: qualified(prefix, enum.GetName()), Comment: comments[key(path)]}
	for i, value := range enum.Value {
		entry.Values = append(entry.Values, protobufEnumValue{
			Number: value.GetNumber(), Name: value.GetName(),
			Comment: comments[key(appendCopy(path, 2, int32(i)))],
		})
	}
	return entry
}

func protobufType(field *descriptorpb.FieldDescriptorProto) string {
	if field.GetTypeName() != "" {
		return strings.TrimPrefix(field.GetTypeName(), ".")
	}
	return strings.TrimPrefix(strings.ToLower(field.GetType().String()), "type_")
}

func qualified(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func appendCopy(path []int32, values ...int32) []int32 {
	result := append([]int32(nil), path...)
	return append(result, values...)
}

func key(path []int32) string {
	parts := make([]string, len(path))
	for i, value := range path {
		parts[i] = fmt.Sprint(value)
	}
	return strings.Join(parts, ".")
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(path, encoded, 0o644)
}

func writeMarkdown(path string, api protobufAPI) error {
	var output bytes.Buffer
	output.WriteString("# Protobuf reference\n\n")
	output.WriteString("Generated from the public Wendy Core descriptor set.\n\n")
	for _, service := range api.Services {
		fmt.Fprintf(&output, "## `%s`\n\n%s\n\n", service.Name, service.Comment)
		for _, method := range service.Methods {
			stream := ""
			if method.ServerStreaming {
				stream = " (server stream)"
			}
			fmt.Fprintf(&output, "### `%s`%s\n\n%s\n\n- Request: `%s`\n- Response: `%s`\n\n", method.Name, stream, method.Comment, method.Input, method.Output)
		}
	}
	for _, message := range api.Messages {
		fmt.Fprintf(&output, "## `%s`\n\n%s\n\n", message.Name, message.Comment)
		if len(message.Fields) == 0 {
			output.WriteString("This message has no fields.\n\n")
			continue
		}
		output.WriteString("| Number | Field | Type | Description |\n|---:|---|---|---|\n")
		for _, field := range message.Fields {
			fieldType := field.Type
			if field.Optional {
				fieldType = "optional " + fieldType
			}
			fmt.Fprintf(&output, "| %d | `%s` | `%s` | %s |\n", field.Number, field.Name, fieldType, field.Comment)
		}
		output.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, output.Bytes(), 0o644)
}

func readManifest(path string) (manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, err
	}
	var result manifest
	if err := json.Unmarshal(raw, &result); err != nil {
		return manifest{}, err
	}
	if result.SchemaVersion != 1 {
		return manifest{}, fmt.Errorf("unsupported manifest schema version: %d", result.SchemaVersion)
	}
	return result, nil
}

func compareFunctions(from, to []cFunction) int {
	old := mapBy(from, func(value cFunction) string { return value.Name })
	current := mapBy(to, func(value cFunction) string { return value.Name })
	return compareNamed("C functions", old, current, func(a, b cFunction) bool { return a.Signature == b.Signature })
}

func compareMessages(from, to []protobufMessage) int {
	old := mapBy(from, func(value protobufMessage) string { return value.Name })
	current := mapBy(to, func(value protobufMessage) string { return value.Name })
	return compareNamed("Protobuf messages", old, current, func(a, b protobufMessage) bool {
		aJSON, _ := json.Marshal(a.Fields)
		bJSON, _ := json.Marshal(b.Fields)
		return bytes.Equal(aJSON, bJSON)
	})
}

func compareServices(from, to []protobufService) int {
	old := mapBy(from, func(value protobufService) string { return value.Name })
	current := mapBy(to, func(value protobufService) string { return value.Name })
	return compareNamed("Protobuf services", old, current, func(a, b protobufService) bool {
		aJSON, _ := json.Marshal(a.Methods)
		bJSON, _ := json.Marshal(b.Methods)
		return bytes.Equal(aJSON, bJSON)
	})
}

func mapBy[T any](values []T, name func(T) string) map[string]T {
	result := make(map[string]T, len(values))
	for _, value := range values {
		result[name(value)] = value
	}
	return result
}

func compareNamed[T any](heading string, old, current map[string]T, equal func(T, T) bool) int {
	var added, removed, changed []string
	for name, before := range old {
		after, ok := current[name]
		if !ok {
			removed = append(removed, name)
		} else if !equal(before, after) {
			changed = append(changed, name)
		}
	}
	for name := range current {
		if _, ok := old[name]; !ok {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	if len(added)+len(removed)+len(changed) == 0 {
		return 0
	}
	fmt.Printf("## %s\n\n", heading)
	for _, name := range added {
		fmt.Printf("- Added `%s`\n", name)
	}
	for _, name := range removed {
		fmt.Printf("- Removed `%s`\n", name)
	}
	for _, name := range changed {
		fmt.Printf("- Changed `%s`\n", name)
	}
	fmt.Println()
	return len(added) + len(removed) + len(changed)
}
