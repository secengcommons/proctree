package workflowpolicy

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

const pinnedCheckout = "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1"
const maxWorkflowFixtureBytes = maxFileBytes

func TestInspectWorkflowFilesAcceptsApprovedActions(t *testing.T) {
	paths := []string{
		workflowFixture(t, "first.yml", "name: First\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{\"uses\": \""+pinnedCheckout+"\"}]}}\n"),
		workflowFixture(t, "second.yml", "name: Second\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: "+pinnedCheckout+" # retained pin\n"),
	}
	if err := inspectWorkflowFiles([]string{paths[1], paths[0]}); err != nil {
		t.Fatal(err)
	}
}

func TestInspectAcceptsRepositoryWorkflows(t *testing.T) {
	paths := repositoryWorkflowPaths(t)
	if err := Inspect(paths); err != nil {
		t.Fatal(err)
	}
}

func TestInspectRejectsInvalidRepositoryInventory(t *testing.T) {
	if err := Inspect(nil); err == nil {
		t.Fatal("empty repository inventory accepted")
	}

	t.Run("duplicate name", func(t *testing.T) {
		paths := copyRepositoryWorkflows(t)
		ci := workflowPath(paths, "ci.yml")
		duplicate := filepath.Join(t.TempDir(), "core.yml")
		source := readOwnedTestFile(t, ci)
		writeOwnedTestFile(t, duplicate, source)
		replaceWorkflowPath(paths, "ci.yml", duplicate)
		if err := Inspect(paths); err == nil {
			t.Fatal("duplicate repository workflow name accepted")
		}
	})

	tests := map[string]struct {
		name        string
		replacement string
	}{
		"missing master": {name: "ci.yml", replacement: "unknown.yml"},
		"missing child":  {name: "core.yml", replacement: "unknown.yml"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			paths := copyRepositoryWorkflows(t)
			path := workflowPath(paths, test.name)
			replacement := filepath.Join(filepath.Dir(path), test.replacement)
			if err := os.Rename(path, replacement); err != nil {
				t.Fatal(err)
			}
			replaceWorkflowPath(paths, test.name, replacement)
			if err := Inspect(paths); err == nil {
				t.Fatal("incomplete repository inventory accepted")
			}
		})
	}
}

func TestInspectRejectsIncompleteRepositoryWorkflows(t *testing.T) {
	tests := map[string]struct {
		file string
		old  string
		new  string
	}{
		"child name":            {file: "core.yml", old: "name: Core\n", new: "name: Changed\n"},
		"child manual dispatch": {file: "core.yml", old: "  workflow_dispatch:\n", new: ""},
		"master name":           {file: "ci.yml", old: "name: CI\n", new: "name: Changed\n"},
		"master merge queue":    {file: "ci.yml", old: "  merge_group:\n", new: ""},
		"master job inventory":  {file: "ci.yml", old: "  required:\n", new: "  extra:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: 'true'\n\n  required:\n"},
		"master required need":  {file: "ci.yml", old: "      - security\n", new: ""},
		"master result check":   {file: "ci.yml", old: "all(.[]; .result == \"success\")", new: "true"},
		"master required steps": {file: "ci.yml", old: "    steps:\n      - name: Check Groups\n", new: "    steps: []\n    ignored:\n"},
		"master group call":     {file: "ci.yml", old: "./.github/workflows/core.yml", new: "./.github/workflows/platforms-containers.yml"},
		"group permissions":     {file: "ci.yml", old: "    permissions:\n      contents: read\n\n  compatibility:", new: "    permissions: read\n\n  compatibility:"},
	}
	for name, mutation := range tests {
		t.Run(name, func(t *testing.T) {
			paths := copyRepositoryWorkflows(t)
			path := filepath.Join(filepath.Dir(paths[0]), mutation.file)
			source := readOwnedTestFile(t, path)
			changed := strings.Replace(string(source), mutation.old, mutation.new, 1)
			if changed == string(source) {
				t.Fatal("mutation did not change workflow")
			}
			writeOwnedTestFile(t, path, []byte(changed))
			if err := Inspect(paths); err == nil {
				t.Fatal("incomplete repository workflow accepted")
			}
		})
	}

	paths := copyRepositoryWorkflows(t)
	if err := Inspect(paths[1:]); err == nil {
		t.Fatal("missing repository workflow accepted")
	}

	paths = copyRepositoryWorkflows(t)
	core := workflowPath(paths, "core.yml")
	if err := os.WriteFile(core, []byte("name: Core\non:\n  workflow_call:\n  schedule:\n    - cron: '0 14 * * 0'\n  workflow_dispatch:\njobs:\n  test:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: 'true'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Inspect(paths); err == nil {
		t.Fatal("repository workflow without an approved action accepted")
	}
}

func FuzzInspectDocument(f *testing.F) {
	f.Add([]byte("name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{uses: " + pinnedCheckout + "}]}}\n"))
	f.Add([]byte("name: &probe Probe\nvalue: *probe\n"))
	f.Add([]byte("---\nname: First\n---\nname: Second\n"))
	f.Fuzz(func(t *testing.T, source []byte) {
		if len(source) > maxFileBytes {
			return
		}
		first, firstErr := inspectDocument(source)
		second, secondErr := inspectDocument(source)
		if first != second || (firstErr == nil) != (secondErr == nil) {
			t.Fatalf("inspection differs: (%d, %v) and (%d, %v)", first, firstErr, second, secondErr)
		}
	})
}

func TestInspectWorkflowFilesRejectsInvalidWorkflows(t *testing.T) {
	tests := map[string]string{
		"alias":            "name: &value Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{uses: " + pinnedCheckout + "}]}}\n",
		"container":        "name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, \"container\": image:latest, steps: [{uses: " + pinnedCheckout + "}]}}\n",
		"duplicate":        "name: Probe\nname: Again\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{uses: " + pinnedCheckout + "}]}}\n",
		"explicit tag":     "name: !!str Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{uses: " + pinnedCheckout + "}]}}\n",
		"local":            "name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{uses: ./.github/actions/probe}]}}\n",
		"merge":            "name: Probe\non: push\ndefaults: &defaults {uses: " + pinnedCheckout + "}\njobs: {test: {runs-on: ubuntu-latest, steps: [{<<: *defaults}]}}\n",
		"multiple":         "name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{uses: " + pinnedCheckout + "}]}}\n---\nname: Trailing\n",
		"invalid trailing": "name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{uses: " + pinnedCheckout + "}]}}\n---\nname: [\n",
		"mutable":          "name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{\"uses\": actions/checkout@main}]}}\n",
		"no action":        "name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{run: 'true'}]}}\n",
		"numeric key":      "name: Probe\non: push\njobs: {1: value, test: {runs-on: ubuntu-latest, steps: [{uses: " + pinnedCheckout + "}]}}\n",
		"services":         "name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, \"services\": {}, steps: [{uses: " + pinnedCheckout + "}]}}\n",
		"tagged action":    "name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{uses: !!str " + pinnedCheckout + "}]}}\n",
		"unapproved":       "name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{uses: attacker/action@0123456789012345678901234567890123456789}]}}\n",
		"invalid":          "name: [\n",
		"empty":            "",
		"comment only":     "# no document\n",
		"empty document":   "---\n...\n",
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if err := inspectWorkflowFiles([]string{workflowFixture(t, "probe.yml", source)}); err == nil {
				t.Fatal("invalid workflow accepted")
			}
		})
	}
}

func TestInspectWorkflowFilesRejectsInvalidFileSets(t *testing.T) {
	valid := workflowFixture(t, "valid.yml", "name: Probe\non: push\njobs: {test: {runs-on: ubuntu-latest, steps: [{uses: "+pinnedCheckout+"}]}}\n")
	if err := inspectWorkflowFiles(nil); err == nil {
		t.Fatal("empty file set accepted")
	}
	if err := inspectWorkflowFiles(make([]string, maxFiles+1)); err == nil {
		t.Fatal("excessive file set accepted")
	}
	if err := inspectWorkflowFiles([]string{valid, valid}); err == nil {
		t.Fatal("duplicate path accepted")
	}
	if err := inspectWorkflowFiles([]string{filepath.Join(t.TempDir(), "missing.yml")}); err == nil {
		t.Fatal("missing workflow accepted")
	}
	if err := inspectWorkflowFiles([]string{t.TempDir()}); err == nil {
		t.Fatal("workflow directory accepted")
	}
	longLine := workflowFixture(t, "long.yml", strings.Repeat("#", maxLineBytes+1)+"\n"+"uses: "+pinnedCheckout+"\n")
	if err := inspectWorkflowFiles([]string{longLine}); err == nil {
		t.Fatal("long workflow line accepted")
	}
	large := workflowFixture(t, "large.yml", strings.Repeat("#\n", maxFileBytes/2+1)+"uses: "+pinnedCheckout+"\n")
	if err := inspectWorkflowFiles([]string{large}); err == nil {
		t.Fatal("large workflow accepted")
	}
	var totalPaths []string
	comment := "# x\n"
	actionLine := "uses: " + pinnedCheckout + "\n"
	for index := range maxTotalBytes/maxFileBytes + 2 {
		source := strings.Repeat(comment, (maxFileBytes-len(actionLine))/len(comment)-1)
		if index == 0 {
			source += actionLine
		}
		totalPaths = append(totalPaths, workflowFixture(t, "total.yml", source))
	}
	if err := inspectWorkflowFiles(totalPaths); err == nil {
		t.Fatal("total workflow source bound accepted")
	}
}

func TestInspectHelpers(t *testing.T) {
	if !boundedLines([]byte(strings.Repeat("x", maxLineBytes))) || boundedLines([]byte(strings.Repeat("x", maxLineBytes+1))) {
		t.Fatal("line boundary differs")
	}
	if lowerHex(strings.Repeat("a", actionRevisionHexBytes)) != true || lowerHex("A") || lowerHex("-") {
		t.Fatal("hex classification differs")
	}
	if _, err := inspectNode(nil); err == nil {
		t.Fatal("nil node accepted")
	}
	if _, err := inspectNode(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"}}}); err == nil {
		t.Fatal("incomplete mapping accepted")
	}
	if _, err := inspectNode(&yaml.Node{Kind: 255}); err == nil {
		t.Fatal("unknown node accepted")
	}
	if _, err := inspectNode(&yaml.Node{}); err == nil {
		t.Fatal("empty node accepted")
	}
	if err := validActionReference(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "actions/checkout@" + strings.Repeat("a", actionRevisionHexBytes) + "@again"}); err == nil {
		t.Fatal("multi-revision action accepted")
	}
}

func TestWorkflowMappingHelpers(t *testing.T) {
	if _, ok := workflowMapping(nil); ok {
		t.Fatal("nil workflow mapping accepted")
	}
	if _, ok := workflowMapping(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"}, {Kind: yaml.ScalarNode}}}); ok {
		t.Fatal("non-string workflow mapping key accepted")
	}
	if exactMappingKeys(map[string]*yaml.Node{"a": {}}, []string{"a", "b"}) || exactMappingKeys(map[string]*yaml.Node{"a": {}}, []string{"b"}) {
		t.Fatal("different mapping keys accepted")
	}
	if exactScalarMapping(map[string]*yaml.Node{"a": {}}, map[string]string{"a": "x", "b": "y"}) ||
		exactScalarMapping(map[string]*yaml.Node{"a": {}}, map[string]string{"a": "x"}) {
		t.Fatal("different scalar mapping accepted")
	}
	if exactScalarSequence(&yaml.Node{Kind: yaml.SequenceNode}, []string{"a"}) ||
		exactScalarSequence(&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "b"}}}, []string{"a"}) {
		t.Fatal("different scalar sequence accepted")
	}
}

func TestRequiredJobRejectsEmptySteps(t *testing.T) {
	source := readOwnedTestFile(t, workflowPath(repositoryWorkflowPaths(t), "ci.yml"))
	document, _, err := inspectDocumentNode(source)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := workflowMapping(document)
	jobs, _ := workflowMapping(root["jobs"])
	required, _ := workflowMapping(jobs["required"])
	required["steps"].Content = nil
	workflows := reusableWorkflows()
	needs := make([]string, 0, len(workflows))
	for _, workflow := range workflows {
		needs = append(needs, workflow.job)
	}
	if validRequiredJob(jobs["required"], needs) {
		t.Fatal("empty required steps accepted")
	}
}

func TestReadWorkflowRetainsCloseAndReadFailures(t *testing.T) {
	if _, err := readWorkflow(filepath.Join(t.TempDir(), "missing"), maxTotalBytes); err == nil {
		t.Fatal("missing workflow accepted")
	}
	if _, err := readWorkflow(workflowFixture(t, "probe.yml", "uses: "+pinnedCheckout+"\n"), -1); err == nil {
		t.Fatal("negative remaining capacity accepted")
	}
}

func TestReadWorkflowRetainsOwnedFileFailures(t *testing.T) {
	path := workflowFixture(t, "probe.yml", "uses: "+pinnedCheckout+"\n")
	failure := errors.New("operation")
	closeFailure := errors.New("close")

	ops := systemWorkflowFileOps()
	ops.openRoot = func(string) (*os.Root, error) { return nil, failure }
	if _, err := readWorkflowWith(path, maxTotalBytes, ops); !errors.Is(err, failure) {
		t.Fatalf("root error = %v", err)
	}

	ops = systemWorkflowFileOps()
	ops.openFile = func(*os.Root, string) (*os.File, error) { return nil, failure }
	ops.closeRoot = func(root *os.Root) error { return errors.Join(root.Close(), closeFailure) }
	if _, err := readWorkflowWith(path, maxTotalBytes, ops); !errors.Is(err, failure) || !errors.Is(err, closeFailure) {
		t.Fatalf("file error = %v", err)
	}

	ops = systemWorkflowFileOps()
	ops.readAll = func(io.Reader) ([]byte, error) { return nil, failure }
	ops.closeFile = func(file *os.File) error { return errors.Join(file.Close(), closeFailure) }
	ops.closeRoot = func(root *os.Root) error { return errors.Join(root.Close(), closeFailure) }
	if _, err := readWorkflowWith(path, maxTotalBytes, ops); !errors.Is(err, failure) || !errors.Is(err, closeFailure) {
		t.Fatalf("read error = %v", err)
	}

	ops = systemWorkflowFileOps()
	ops.readAll = func(io.Reader) ([]byte, error) { return make([]byte, maxFileBytes+1), nil }
	if _, err := readWorkflowWith(path, maxTotalBytes, ops); err == nil {
		t.Fatal("post-open source growth accepted")
	}
}

func workflowFixture(t *testing.T, name, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func repositoryWorkflowPaths(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func copyRepositoryWorkflows(t *testing.T) []string {
	t.Helper()
	directory := t.TempDir()
	paths := repositoryWorkflowPaths(t)
	copies := make([]string, 0, len(paths))
	for _, path := range paths {
		destination := filepath.Join(directory, filepath.Base(path))
		source := readOwnedTestFile(t, path)
		writeOwnedTestFile(t, destination, source)
		copies = append(copies, destination)
	}
	return copies
}

func readOwnedTestFile(t *testing.T, path string) []byte {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		t.Fatal(errors.Join(err, root.Close()))
	}
	source, readErr := io.ReadAll(io.LimitReader(file, maxWorkflowFixtureBytes+1))
	if readErr == nil && len(source) > maxWorkflowFixtureBytes {
		readErr = errors.New("workflow fixture exceeds its bound")
	}
	if err := errors.Join(readErr, file.Close(), root.Close()); err != nil {
		t.Fatal(err)
	}
	return source
}

func writeOwnedTestFile(t *testing.T, path string, source []byte) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile(filepath.Base(path), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(errors.Join(err, root.Close()))
	}
	written, writeErr := file.Write(source)
	if writeErr == nil && written != len(source) {
		writeErr = io.ErrShortWrite
	}
	if err := errors.Join(writeErr, file.Close(), root.Close()); err != nil {
		t.Fatal(err)
	}
}

func workflowPath(paths []string, name string) string {
	for _, path := range paths {
		if filepath.Base(path) == name {
			return path
		}
	}
	return ""
}

func replaceWorkflowPath(paths []string, name, replacement string) {
	for index, path := range paths {
		if filepath.Base(path) == name {
			paths[index] = replacement
			return
		}
	}
}

func TestErrorIdentityDoesNotHideFileFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yml")
	if err := inspectWorkflowFiles([]string{path}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error = %v", err)
	}
}
