package workflowpolicy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
)

const maxFiles = 32
const maxFileBytes = 64 << 10
const maxLineBytes = 4 << 10
const maxTotalBytes = 256 << 10
const actionRevisionHexBytes = 40

type workflowFileOps struct {
	lstat     func(string) (os.FileInfo, error)
	openRoot  func(string) (*os.Root, error)
	openFile  func(*os.Root, string) (*os.File, error)
	readAll   func(io.Reader) ([]byte, error)
	closeFile func(*os.File) error
	closeRoot func(*os.Root) error
}

type reusableWorkflow struct {
	job  string
	file string
	name string
}

type inspectedWorkflow struct {
	path     string
	document *yaml.Node
	uses     int
}

func systemWorkflowFileOps() workflowFileOps {
	return workflowFileOps{
		lstat: os.Lstat, openRoot: os.OpenRoot, openFile: func(root *os.Root, name string) (*os.File, error) { return root.Open(name) },
		readAll: io.ReadAll, closeFile: func(file *os.File) error { return file.Close() }, closeRoot: func(root *os.Root) error { return root.Close() },
	}
}

func Inspect(paths []string) error {
	workflows, err := inspectWorkflows(paths)
	if err != nil {
		return err
	}
	return inspectRepositoryWorkflows(workflows)
}

func inspectWorkflowFiles(paths []string) error {
	_, err := inspectWorkflows(paths)
	return err
}

func inspectWorkflows(paths []string) ([]inspectedWorkflow, error) {
	if len(paths) == 0 || len(paths) > maxFiles {
		return nil, errors.New("workflow file count is outside its bound")
	}
	ordered := append([]string(nil), paths...)
	slices.Sort(ordered)
	total := 0
	uses := 0
	workflows := make([]inspectedWorkflow, 0, len(ordered))
	for index, path := range ordered {
		if index > 0 && path == ordered[index-1] {
			return nil, fmt.Errorf("workflow path is duplicated: %q", path)
		}
		data, err := readWorkflow(path, maxTotalBytes-total)
		if err != nil {
			return nil, err
		}
		total += len(data)
		document, count, err := inspectDocumentNode(data)
		if err != nil {
			return nil, fmt.Errorf("inspect workflow %q: %w", path, err)
		}
		uses += count
		workflows = append(workflows, inspectedWorkflow{path: path, document: document, uses: count})
	}
	if uses == 0 {
		return nil, errors.New("workflow set contains no approved action")
	}
	return workflows, nil
}

func readWorkflow(path string, remaining int) ([]byte, error) {
	return readWorkflowWith(path, remaining, systemWorkflowFileOps())
}

func readWorkflowWith(path string, remaining int, ops workflowFileOps) ([]byte, error) {
	information, err := ops.lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect workflow %q: %w", path, err)
	}
	if !information.Mode().IsRegular() || information.Size() < 0 || information.Size() > maxFileBytes || information.Size() > int64(remaining) {
		return nil, fmt.Errorf("workflow %q is not a bounded regular file", path)
	}
	root, err := ops.openRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open workflow %q: %w", path, err)
	}
	file, err := ops.openFile(root, filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("open workflow %q: %w", path, errors.Join(err, ops.closeRoot(root)))
	}
	data, readErr := ops.readAll(io.LimitReader(file, int64(maxFileBytes)+1))
	closeErr := errors.Join(ops.closeFile(file), ops.closeRoot(root))
	if err = errors.Join(readErr, closeErr); err != nil {
		return nil, fmt.Errorf("read workflow %q: %w", path, err)
	}
	if len(data) > maxFileBytes || len(data) > remaining || !boundedLines(data) {
		return nil, fmt.Errorf("workflow %q exceeds its source bound", path)
	}
	return data, nil
}

func boundedLines(data []byte) bool {
	lineStart := 0
	for index, value := range data {
		if value == '\n' {
			if index-lineStart > maxLineBytes {
				return false
			}
			lineStart = index + 1
		}
	}
	return len(data)-lineStart <= maxLineBytes
}

func inspectDocument(data []byte) (int, error) {
	_, uses, err := inspectDocumentNode(data)
	return uses, err
}

func inspectDocumentNode(data []byte) (*yaml.Node, int, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, 0, err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, 0, errors.New("workflow contains multiple YAML documents")
		}
		return nil, 0, err
	}
	uses, err := inspectNode(document.Content[0])
	return document.Content[0], uses, err
}

func inspectNode(node *yaml.Node) (int, error) {
	if node == nil || node.Kind == yaml.AliasNode || node.Alias != nil || node.Anchor != "" || node.Style&yaml.TaggedStyle != 0 {
		return 0, errors.New("workflow contains an unsupported YAML construct")
	}
	switch node.Kind {
	case yaml.MappingNode:
		return inspectMapping(node)
	case yaml.SequenceNode, yaml.DocumentNode:
		return inspectChildren(node.Content)
	case yaml.ScalarNode:
		return 0, nil
	case yaml.AliasNode, 0:
		return 0, errors.New("workflow contains an unsupported YAML node")
	default:
		return 0, errors.New("workflow contains an unsupported YAML node")
	}
}

func inspectMapping(node *yaml.Node) (int, error) {
	if len(node.Content)%2 != 0 {
		return 0, errors.New("workflow mapping is incomplete")
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	uses := 0
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Anchor != "" || key.Style&yaml.TaggedStyle != 0 {
			return 0, errors.New("workflow mapping key is not an ordinary string")
		}
		if _, found := seen[key.Value]; found {
			return 0, fmt.Errorf("workflow mapping key is duplicated: %q", key.Value)
		}
		seen[key.Value] = struct{}{}
		switch key.Value {
		case "container", "services":
			return 0, fmt.Errorf("workflow %s execution is not permitted", key.Value)
		case "uses":
			if err := validActionReference(value); err != nil {
				return 0, err
			}
			uses++
		}
		count, err := inspectNode(value)
		if err != nil {
			return 0, err
		}
		uses += count
	}
	return uses, nil
}

func inspectChildren(children []*yaml.Node) (int, error) {
	uses := 0
	for _, child := range children {
		count, err := inspectNode(child)
		if err != nil {
			return 0, err
		}
		uses += count
	}
	return uses, nil
}

func validActionReference(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" || node.Anchor != "" || node.Style&yaml.TaggedStyle != 0 {
		return errors.New("workflow action reference is not an ordinary string")
	}
	if approvedReusableWorkflow(node.Value) {
		return nil
	}
	action, revision, found := strings.Cut(node.Value, "@")
	if !found || strings.Contains(revision, "@") || len(revision) != actionRevisionHexBytes || !lowerHex(revision) {
		return fmt.Errorf("workflow action is not pinned to a full commit: %q", node.Value)
	}
	if !approvedAction(action) {
		return fmt.Errorf("workflow action is not approved: %q", action)
	}
	return nil
}

func approvedReusableWorkflow(value string) bool {
	for _, workflow := range reusableWorkflows() {
		if value == "./.github/workflows/"+workflow.file {
			return true
		}
	}
	return false
}

func reusableWorkflows() []reusableWorkflow {
	return []reusableWorkflow{
		{job: "core", file: "core.yml", name: "Core"},
		{job: "compatibility", file: "compatibility.yml", name: "Compatibility"},
		{job: "security", file: "security.yml", name: "Security"},
		{job: "fuzzing", file: "fuzzing.yml", name: "Fuzzing"},
		{job: "ports", file: "ports.yml", name: "Go Ports"},
		{job: "platforms-containers", file: "platforms-containers.yml", name: "Platforms: Containers"},
		{job: "platforms-native", file: "platforms-native.yml", name: "Platforms: Native"},
		{job: "platforms-virtual", file: "platforms-virtual.yml", name: "Platforms: Virtual"},
		{job: "platforms-solaris", file: "platforms-solaris.yml", name: "Platforms: Solaris"},
	}
}

func inspectRepositoryWorkflows(workflows []inspectedWorkflow) error {
	expected := reusableWorkflows()
	if len(workflows) != len(expected)+1 {
		return errors.New("repository workflow inventory differs")
	}
	byName := make(map[string]inspectedWorkflow, len(workflows))
	for _, workflow := range workflows {
		name := filepath.Base(workflow.path)
		if _, found := byName[name]; found {
			return fmt.Errorf("repository workflow name is duplicated: %q", name)
		}
		byName[name] = workflow
	}
	master, found := byName["ci.yml"]
	if !found {
		return errors.New("repository CI workflow is missing")
	}
	delete(byName, "ci.yml")
	if err := inspectMasterWorkflow(master.document, expected); err != nil {
		return fmt.Errorf("inspect repository CI workflow: %w", err)
	}
	for _, expectedWorkflow := range expected {
		workflow, found := byName[expectedWorkflow.file]
		if !found {
			return fmt.Errorf("repository workflow is missing: %q", expectedWorkflow.file)
		}
		delete(byName, expectedWorkflow.file)
		if workflow.uses == 0 {
			return fmt.Errorf("repository workflow contains no approved action: %q", expectedWorkflow.file)
		}
		if err := inspectChildWorkflow(workflow.document, expectedWorkflow.name); err != nil {
			return fmt.Errorf("inspect repository workflow %q: %w", expectedWorkflow.file, err)
		}
	}
	return nil
}

func inspectChildWorkflow(document *yaml.Node, expectedName string) error {
	root, ok := workflowMapping(document)
	if !ok || !scalarValue(root["name"], expectedName) {
		return errors.New("workflow name differs")
	}
	triggers, ok := workflowMapping(root["on"])
	if !ok || !exactMappingKeys(triggers, []string{"schedule", "workflow_call", "workflow_dispatch"}) {
		return errors.New("workflow triggers differ")
	}
	return nil
}

func inspectMasterWorkflow(document *yaml.Node, expected []reusableWorkflow) error {
	root, ok := workflowMapping(document)
	if !ok || !scalarValue(root["name"], "CI") {
		return errors.New("master workflow name differs")
	}
	triggers, ok := workflowMapping(root["on"])
	if !ok || !exactMappingKeys(triggers, []string{"merge_group", "pull_request", "push", "workflow_dispatch"}) {
		return errors.New("master workflow triggers differ")
	}
	jobs, ok := workflowMapping(root["jobs"])
	if !ok || len(jobs) != len(expected)+1 {
		return errors.New("master workflow job inventory differs")
	}
	needs := make([]string, 0, len(expected))
	for _, expectedWorkflow := range expected {
		job, found := jobs[expectedWorkflow.job]
		if !found || !validReusableJob(job, expectedWorkflow) {
			return fmt.Errorf("master workflow call differs: %q", expectedWorkflow.job)
		}
		needs = append(needs, expectedWorkflow.job)
	}
	if !validRequiredJob(jobs["required"], needs) {
		return errors.New("master required job differs")
	}
	return nil
}

func validReusableJob(node *yaml.Node, expected reusableWorkflow) bool {
	job, ok := workflowMapping(node)
	if !ok || !exactMappingKeys(job, []string{"name", "permissions", "uses"}) ||
		!scalarValue(job["name"], expected.name) ||
		!scalarValue(job["uses"], "./.github/workflows/"+expected.file) {
		return false
	}
	permissions, ok := workflowMapping(job["permissions"])
	if !ok {
		return false
	}
	if expected.job == "security" {
		return exactScalarMapping(permissions, map[string]string{
			"actions": "read", "contents": "read", "security-events": "write",
		})
	}
	return exactScalarMapping(permissions, map[string]string{"contents": "read"})
}

func validRequiredJob(node *yaml.Node, expectedNeeds []string) bool {
	job, ok := workflowMapping(node)
	if !ok || !validRequiredJobHeader(job, expectedNeeds) {
		return false
	}
	if job["steps"] == nil || job["steps"].Kind != yaml.SequenceNode || len(job["steps"].Content) != 1 {
		return false
	}
	step, ok := workflowMapping(job["steps"].Content[0])
	if !ok || !validRequiredStep(step) {
		return false
	}
	environment, ok := workflowMapping(step["env"])
	return ok && exactScalarMapping(environment, map[string]string{"RESULTS": "${{ toJSON(needs) }}"})
}

func validRequiredJobHeader(job map[string]*yaml.Node, expectedNeeds []string) bool {
	return exactMappingKeys(job, []string{"if", "name", "needs", "permissions", "runs-on", "steps", "timeout-minutes"}) &&
		scalarValue(job["name"], "Required") &&
		scalarValue(job["if"], "${{ always() }}") &&
		scalarValue(job["runs-on"], "ubuntu-24.04") &&
		scalarValue(job["timeout-minutes"], "5") &&
		emptyMapping(job["permissions"]) &&
		exactScalarSequence(job["needs"], expectedNeeds)
}

func validRequiredStep(step map[string]*yaml.Node) bool {
	return exactMappingKeys(step, []string{"env", "name", "run", "shell"}) &&
		scalarValue(step["name"], "Check Groups") &&
		scalarValue(step["shell"], "bash") &&
		scalarValue(step["run"], requiredGroupScript())
}

func requiredGroupScript() string {
	return "if jq -e 'length > 0 and all(.[]; .result == \"success\")' <<<\"$RESULTS\" >/dev/null; then\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf 'One or more CI groups failed\\n' >&2\n" +
		"jq -r 'to_entries[] | \"\\(.key): \\(.value.result)\"' <<<\"$RESULTS\" >&2\n" +
		"exit 1\n"
}

func workflowMapping(node *yaml.Node) (map[string]*yaml.Node, bool) {
	if node == nil || node.Kind != yaml.MappingNode || len(node.Content)%2 != 0 {
		return nil, false
	}
	values := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return nil, false
		}
		values[key.Value] = node.Content[index+1]
	}
	return values, true
}

func exactMappingKeys(mapping map[string]*yaml.Node, expected []string) bool {
	if len(mapping) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, found := mapping[key]; !found {
			return false
		}
	}
	return true
}

func exactScalarMapping(mapping map[string]*yaml.Node, expected map[string]string) bool {
	if len(mapping) != len(expected) {
		return false
	}
	for key, value := range expected {
		if !scalarValue(mapping[key], value) {
			return false
		}
	}
	return true
}

func scalarValue(node *yaml.Node, expected string) bool {
	return node != nil && node.Kind == yaml.ScalarNode && node.Value == expected
}

func emptyMapping(node *yaml.Node) bool {
	return node != nil && node.Kind == yaml.MappingNode && len(node.Content) == 0
}

func exactScalarSequence(node *yaml.Node, expected []string) bool {
	if node == nil || node.Kind != yaml.SequenceNode || len(node.Content) != len(expected) {
		return false
	}
	for index, value := range expected {
		if !scalarValue(node.Content[index], value) {
			return false
		}
	}
	return true
}

func lowerHex(value string) bool {
	for _, character := range value {
		decimal := character >= '0' && character <= '9'
		hexadecimal := character >= 'a' && character <= 'f'
		if !decimal && !hexadecimal {
			return false
		}
	}
	return true
}

func approvedAction(action string) bool {
	switch action {
	case "actions/checkout",
		"actions/dependency-review-action",
		"actions/setup-go",
		"cross-platform-actions/action",
		"github/codeql-action/analyze",
		"github/codeql-action/init",
		"vmactions/solaris-vm":
		return true
	default:
		return false
	}
}
