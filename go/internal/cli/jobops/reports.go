package jobops

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const maxProjectedReport = 1024 * 1024
const maxProjectionBytes = 8192
const workflowReceiptName = ".wendy-artifacts-workflow-receipt.json"

func validateReportFields(files []string, selections map[string][]string) error {
	if len(selections) > 8 {
		return fmt.Errorf("select at most eight JSON reports")
	}
	allowed := map[string]bool{}
	for _, name := range files {
		allowed[name] = true
	}
	count := 0
	for name, pointers := range selections {
		if !allowed[name] || len(pointers) == 0 {
			return fmt.Errorf("report_fields must select fields from retrieved artifact_files")
		}
		seen := map[string]bool{}
		for _, pointer := range pointers {
			count++
			if len(pointer) > 128 || seen[pointer] {
				return fmt.Errorf("report_fields requires unique JSON pointers of at most 128 bytes")
			}
			if _, err := pointerParts(pointer); err != nil {
				return err
			}
			seen[pointer] = true
		}
	}
	if count > 32 {
		return fmt.Errorf("select at most 32 report fields")
	}
	return nil
}

func pointerParts(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("report_fields requires RFC 6901 JSON pointers")
	}
	parts := strings.Split(pointer[1:], "/")
	for i, part := range parts {
		for j := 0; j < len(part); j++ {
			if part[j] == '~' {
				if j+1 == len(part) || (part[j+1] != '0' && part[j+1] != '1') {
					return nil, fmt.Errorf("invalid JSON pointer escape")
				}
				j++
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

func projectScalar(document any, pointer string) (any, string) {
	parts, err := pointerParts(pointer)
	if err != nil {
		return nil, "invalid_pointer"
	}
	value := document
	for _, key := range parts {
		switch container := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = container[key]
			if !ok {
				return nil, "missing_pointer"
			}
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || strconv.Itoa(index) != key || index < 0 || index >= len(container) {
				return nil, "missing_pointer"
			}
			value = container[index]
		default:
			return nil, "missing_pointer"
		}
	}
	switch scalar := value.(type) {
	case nil, bool:
		return value, ""
	case string:
		if len(scalar) <= 160 {
			return scalar, ""
		}
	case json.Number:
		n, err := scalar.Float64()
		if err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) {
			return scalar, ""
		}
	}
	return nil, "not_bounded_scalar"
}

// readRootRegular pins reads to the selected output root and refuses FIFO,
// symlink and oversized leaf files before parsing any report content.
func readRootRegular(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("report is not a bounded regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|regularReadFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() > limit {
		return nil, fmt.Errorf("report changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("report exceeds projection read limit")
	}
	return data, nil
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func summarizeReports(destination string, artifacts map[string]any, selections map[string][]string) (map[string]any, error) {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	raw, err := readRootRegular(root, ".wendy-artifacts-manifest.json", maxProjectedReport)
	if err != nil {
		return nil, err
	}
	if digest(raw) != artifacts["manifest_sha256"] {
		return nil, fmt.Errorf("artifact manifest changed before report projection")
	}
	var manifest struct {
		Files []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("invalid verified artifact manifest")
	}
	hashes := map[string]string{}
	for _, file := range manifest.Files {
		hashes[file.Path] = file.SHA256
	}
	result := map[string]any{}
	names := make([]string, 0, len(selections))
	for name := range selections {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fields := map[string]any{}
		unavailable := map[string]string{}
		report := map[string]any{"sha256": hashes[name], "fields": fields, "unavailable": unavailable}
		result[name] = report
		data, err := readRootRegular(root, name, maxProjectedReport)
		problem := ""
		var document any
		if err != nil {
			problem = "unreadable_or_oversized_report"
		} else if hashes[name] == "" || digest(data) != hashes[name] {
			problem = "report_hash_mismatch"
		} else {
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			if decoder.Decode(&document) != nil {
				problem = "invalid_json"
			} else {
				var extra any
				if decoder.Decode(&extra) != io.EOF {
					problem = "invalid_json"
				}
			}
		}
		pointers := append([]string(nil), selections[name]...)
		sort.Strings(pointers)
		for _, pointer := range pointers {
			if problem != "" {
				unavailable[pointer] = problem
				continue
			}
			value, reason := projectScalar(document, pointer)
			if reason != "" {
				unavailable[pointer] = reason
				continue
			}
			fields[pointer] = value
			encoded, _ := json.Marshal(result)
			if len(encoded) > maxProjectionBytes {
				delete(fields, pointer)
				unavailable[pointer] = "projection_budget_exceeded"
			}
		}
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) > maxProjectionBytes {
		return nil, fmt.Errorf("selected report references exceed the 8192-byte projection budget")
	}
	return result, nil
}

func saveWorkflowReceipt(destination string, receipt map[string]any) error {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	name := ".wendy-artifacts-receipt-" + rand.Text()
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = root.Rename(name, workflowReceiptName); err != nil {
		return err
	}
	receipt["receipt_path"] = filepath.Join(destination, workflowReceiptName)
	receipt["receipt_sha256"] = digest(data)
	return nil
}
