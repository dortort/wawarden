package archtest

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func yamlEntries(block string) []string {
	var entries [][]string
	base := -1
	for line := range strings.Lines(block) {
		text := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if text != "" && !strings.HasPrefix(text, "#") {
			if base < 0 {
				base = indent
			}
			if indent == base {
				entries = append(entries, nil)
			}
		}
		if len(entries) > 0 {
			entries[len(entries)-1] = append(entries[len(entries)-1], line)
		}
	}
	var out []string
	for _, lines := range entries {
		cut, head := base, lines[0][base:]
		if item, ok := strings.CutPrefix(head, "- "); ok {
			cut, head = base+2, item
		}
		var b strings.Builder
		b.WriteString(head)
		for _, line := range lines[1:] {
			if len(line)-len(strings.TrimLeft(line, " ")) >= cut {
				b.WriteString(line[cut:])
			} else {
				b.WriteString(strings.TrimLeft(line, " "))
			}
		}
		out = append(out, b.String())
	}
	return out
}

func yamlBlock(config string, keys ...string) string {
	for _, key := range keys {
		next := ""
		for _, entry := range yamlEntries(config) {
			if k, _, _ := strings.Cut(entry, ":"); k == key {
				_, next, _ = strings.Cut(entry, "\n")
			}
		}
		config = next
	}
	return config
}

func TestYAMLEntries(t *testing.T) {
	const config = `jobs:
  a:
    steps:
      - name: one
        run: |
          echo one

          echo two
      # comment
      - uses: x@0
        with:
          k: v
  b:
    if: c
`
	if got, want := yamlEntries(yamlBlock(config, "jobs")), []string{
		"a:\n  steps:\n    - name: one\n      run: |\n        echo one\n\n        echo two\n    # comment\n    - uses: x@0\n      with:\n        k: v\n",
		"b:\n  if: c\n",
	}; !slices.Equal(got, want) {
		t.Fatalf("jobs %q, want %q", got, want)
	}
	if got, want := yamlEntries(yamlBlock(config, "jobs", "a", "steps")), []string{
		"name: one\nrun: |\n  echo one\n\n  echo two\n# comment\n",
		"uses: x@0\nwith:\n  k: v\n",
	}; !slices.Equal(got, want) {
		t.Fatalf("steps %q, want %q", got, want)
	}
	if got := yamlBlock(config, "jobs", "c", "steps"); got != "" {
		t.Fatalf("block of a missing key %q, want none", got)
	}
}

type workflowJob struct {
	name  string
	steps []string
}

func workflowJobs(config string) []workflowJob {
	var out []workflowJob
	for _, entry := range yamlEntries(yamlBlock(config, "jobs")) {
		name, _, _ := strings.Cut(entry, ":")
		out = append(out, workflowJob{name: name, steps: yamlEntries(yamlBlock(entry, name, "steps"))})
	}
	return out
}

func stepScript(step string) (string, bool) {
	inline, found := yamlValue(step, "run")
	return inline + "\n" + yamlBlock(step, "run"), found
}

func workflowScripts(config string) []string {
	var out []string
	for _, job := range workflowJobs(config) {
		for _, step := range job.steps {
			if script, found := stepScript(step); found {
				out = append(out, script)
			}
		}
	}
	return out
}

func TestWorkflowScriptsTakeNoExpressions(t *testing.T) {
	root := os.DirFS(moduleRoot(t))
	workflows, err := fs.Glob(root, ".github/workflows/*")
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	checked := 0
	for _, name := range workflows {
		data, err := fs.ReadFile(root, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, script := range workflowScripts(string(data)) {
			checked++
			for line := range strings.Lines(script) {
				if strings.Contains(line, "${{") {
					t.Errorf("%s: run script line %q holds an expression, which the runner pastes into the script before the shell parses it; pass the value through env instead", name, strings.TrimSpace(line))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no workflow has a run script, so this check passes vacuously")
	}
}

func TestWorkflowScripts(t *testing.T) {
	const config = `env:
  VERSION: ${{ inputs.version }}
defaults:
  run:
    shell: bash
jobs:
  a:
    name: A ${{ matrix.x }}
    steps:
      - run: echo one
      - name: two
        env:
          X: ${{ inputs.x }}
        run: |
          echo two
          echo "$X"
      - uses: actions/checkout@0000000000000000000000000000000000000000 # v0
        with:
          ref: ${{ inputs.ref }}
  b:
    steps:
      - name: three
        run: >-
          echo
          three
`
	for _, tt := range []struct {
		name, old, new string
		want           int
	}{
		{name: "expressions outside scripts"},
		{name: "one-line script", old: "echo one", new: "echo ${{ inputs.x }}", want: 1},
		{name: "literal block", old: `echo "$X"`, new: `echo "${{ inputs.x }}"`, want: 1},
		{name: "folded block", old: "          three", new: "          ${{ inputs.x }}", want: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scripts := workflowScripts(strings.Replace(config, tt.old, tt.new, 1))
			if len(scripts) != 3 {
				t.Fatalf("%d scripts, want 3: %q", len(scripts), scripts)
			}
			if got := len(slices.DeleteFunc(scripts, func(s string) bool { return !strings.Contains(s, "${{") })); got != tt.want {
				t.Fatalf("%d scripts with an expression, want %d", got, tt.want)
			}
		})
	}
}

var bumpJobs = map[string]struct {
	condition   string
	permissions []string
}{
	"prepare": {condition: "github.ref == 'refs/heads/main'", permissions: []string{"contents: read"}},
	"propose": {condition: "github.ref == 'refs/heads/main' && needs.prepare.outputs.new != ''", permissions: []string{"contents: write", "pull-requests: write"}},
}

const openPullRequestLookup = `number=$(gh pr list --head "$branch" --base main --state open --json number,isCrossRepository --jq 'map(select(.isCrossRepository | not)) | .[0].number // empty')`

var (
	goCommand = regexp.MustCompile(`(?m)\bgo(?:\s|$)`)
	ghPR      = regexp.MustCompile(`\bgh pr (\S+)(?: (\S+))?`)
)

func bumpWorkflowProblems(config string) []string {
	var out []string
	if got, _ := yamlValue(config, "permissions"); got != "{}" {
		out = append(out, fmt.Sprintf("permissions is %q, want {}, so that a job holds only the permissions it names", got))
	}
	var names []string
	lookups := 0
	for _, job := range workflowJobs(config) {
		names = append(names, job.name)
		want, known := bumpJobs[job.name]
		if got, _ := yamlValue(config, "jobs", job.name, "if"); known && got != want.condition {
			out = append(out, fmt.Sprintf("jobs.%s.if is %q, want %q, so that the job runs from main only", job.name, got, want.condition))
		}
		inline, _ := yamlValue(config, "jobs", job.name, "permissions")
		var granted []string
		for line := range strings.Lines(yamlBlock(config, "jobs", job.name, "permissions")) {
			if text := strings.TrimSpace(line); text != "" && !strings.HasPrefix(text, "#") {
				granted = append(granted, text)
			}
		}
		if inline != "" || (known && !slices.Equal(granted, want.permissions)) {
			out = append(out, fmt.Sprintf("jobs.%s.permissions is %q %q, want exactly %q", job.name, inline, granted, want.permissions))
		}
		writes := strings.Contains(inline, "write") || slices.ContainsFunc(granted, func(p string) bool { return strings.Contains(p, "write") })
		setup, firstGo := -1, -1
		for i, step := range job.steps {
			uses, _ := yamlValue(step, "uses")
			script, _ := stepScript(step)
			if firstGo < 0 && goCommand.MatchString(script) {
				firstGo = i
			}
			if strings.HasPrefix(uses, "actions/setup-go@") {
				setup = i
			}
			if strings.HasPrefix(uses, "actions/checkout@") {
				if got, _ := yamlValue(step, "with", "persist-credentials"); got != "false" {
					out = append(out, fmt.Sprintf("jobs.%s step %d checks out with persist-credentials %q, want false, so that no later step can push with the token", job.name, i+1, got))
				}
			}
			if writes && uses != "" && !strings.HasPrefix(uses, "actions/checkout@") && !strings.HasPrefix(uses, "actions/download-artifact@") {
				out = append(out, fmt.Sprintf("jobs.%s step %d uses %s, yet the job can write: it runs only checkout and download-artifact", job.name, i+1, uses))
			}
			if writes && (goCommand.MatchString(script) || strings.Contains(script, "hack/")) {
				out = append(out, fmt.Sprintf("jobs.%s step %d runs Go or a hack/ script, yet the job can write: it runs no Go and no upstream code", job.name, i+1))
			}
			for line := range strings.Lines(script) {
				for _, m := range ghPR.FindAllStringSubmatch(line, -1) {
					switch {
					case m[1] == "list":
						lookups++
						if strings.TrimSpace(line) != openPullRequestLookup {
							out = append(out, fmt.Sprintf("jobs.%s step %d lists pull requests with %q, want %s, which skips pull requests from forks", job.name, i+1, strings.TrimSpace(line), openPullRequestLookup))
						}
					case m[1] != "create" && m[2] != `"$number"`:
						out = append(out, fmt.Sprintf("jobs.%s step %d runs gh pr %s on %q, not on the pull request that the lookup found in this repository", job.name, i+1, m[1], m[2]))
					}
				}
			}
		}
		if setup >= 0 || firstGo >= 0 {
			check := ""
			if setup >= 0 && setup+1 < len(job.steps) {
				check, _ = yamlValue(job.steps[setup+1], "run")
			}
			if check != "hack/check-go-version.sh" {
				out = append(out, fmt.Sprintf("jobs.%s runs Go but does not run hack/check-go-version.sh in the step right after actions/setup-go", job.name))
			}
		}
		if firstGo >= 0 && firstGo < setup {
			out = append(out, fmt.Sprintf("jobs.%s step %d runs Go before actions/setup-go, on the runner's own Go, which hack/check-go-version.sh does not check", job.name, firstGo+1))
		}
	}
	slices.Sort(names)
	if want := slices.Sorted(maps.Keys(bumpJobs)); !slices.Equal(names, want) {
		out = append(out, fmt.Sprintf("jobs %q, want %q", names, want))
	}
	if lookups == 0 {
		out = append(out, "no step looks up the open pull request from the workflow's branch")
	}
	return out
}

func TestBumpWorkflowIsolation(t *testing.T) {
	const name = ".github/workflows/bump-whatsmeow.yml"
	data, err := fs.ReadFile(os.DirFS(moduleRoot(t)), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	for _, problem := range bumpWorkflowProblems(string(data)) {
		t.Errorf("%s: %s", name, problem)
	}
}

func TestBumpWorkflowProblems(t *testing.T) {
	const config = `permissions: {}
jobs:
  prepare:
    if: github.ref == 'refs/heads/main'
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@0000000000000000000000000000000000000000 # v0
        with:
          persist-credentials: false
      - uses: actions/setup-go@0000000000000000000000000000000000000000 # v0
      - name: Check the Go version
        run: hack/check-go-version.sh
      - name: Build
        run: |
          go build ./...
          hack/offline-test.sh
  propose:
    needs: prepare
    if: github.ref == 'refs/heads/main' && needs.prepare.outputs.new != ''
    permissions:
      contents: write
      pull-requests: write
    steps:
      - name: Check out
        uses: actions/checkout@0000000000000000000000000000000000000000 # v0
        with:
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/download-artifact@0000000000000000000000000000000000000000 # v0
      - name: Propose
        run: |
          git push origin HEAD:refs/heads/bump/whatsmeow
          ` + openPullRequestLookup + `
          if [ -n "$number" ]; then
            gh pr edit "$number" --body-file body.md
          elif ! gh pr create --base main --head "$branch" --body-file body.md; then
            exit 1
          fi
`
	const setupGo = "      - uses: actions/setup-go@0000000000000000000000000000000000000000 # v0\n"
	const check = "      - name: Check the Go version\n        run: hack/check-go-version.sh\n"
	for _, tt := range []struct {
		name, old, new string
		want           int
	}{
		{name: "the reviewed workflow"},
		{name: "workflow-wide permissions", old: "permissions: {}", new: "permissions: read-all", want: 1},
		{name: "prepare on any branch", old: "    if: github.ref == 'refs/heads/main'\n", want: 1},
		{name: "propose on any branch", old: "github.ref == 'refs/heads/main' && needs", new: "needs", want: 1},
		{name: "prepare can write", old: "contents: read", new: "contents: write", want: 4},
		{name: "propose can write more", old: "      pull-requests: write\n", new: "      pull-requests: write\n      actions: write\n", want: 1},
		{name: "propose with write-all", old: "    permissions:\n      contents: write\n      pull-requests: write\n", new: "    permissions: write-all\n", want: 1},
		{name: "a third job", old: "  propose:\n", new: "  other:\n    if: github.ref == 'refs/heads/main'\n    steps:\n      - run: echo\n  propose:\n", want: 1},
		{name: "no propose job", old: "  propose:\n", new: "  publish:\n", want: 1},
		{name: "Go in the write job", old: "          git push", new: "          go build ./...\n          git push", want: 2},
		{name: "setup-go in the write job", old: "      - name: Propose\n", new: setupGo + "      - name: Propose\n", want: 2},
		{name: "a hack script in the write job", old: "          git push", new: "          hack/offline-test.sh\n          git push", want: 1},
		{name: "persisted credentials", old: "          fetch-depth: 0\n          persist-credentials: false\n", new: "          fetch-depth: 0\n          persist-credentials: true\n", want: 1},
		{name: "default credentials", old: "        with:\n          persist-credentials: false\n", want: 1},
		{name: "no Go version check", old: check, want: 1},
		{name: "Go used before the version check", old: setupGo + check, new: setupGo + "      - run: go version\n" + check, want: 1},
		{name: "Go without setup-go", old: setupGo + check, want: 1},
		{name: "Go before setup-go", old: setupGo, new: "      - run: go mod download\n" + setupGo, want: 1},
		{name: "fork pull requests", old: "--json number,isCrossRepository --jq 'map(select(.isCrossRepository | not)) | .[0].number // empty'", new: "--json number --jq '.[0].number // empty'", want: 1},
		{name: "no lookup", old: "          " + openPullRequestLookup + "\n", want: 1},
		{name: "edit by branch", old: `gh pr edit "$number"`, new: `gh pr edit "$branch"`, want: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mutated := config
			if tt.old != "" {
				if !strings.Contains(config, tt.old) {
					t.Fatalf("the reviewed workflow has no %q", tt.old)
				}
				mutated = strings.Replace(config, tt.old, tt.new, 1)
			}
			if got := bumpWorkflowProblems(mutated); len(got) != tt.want {
				t.Fatalf("%d problems, want %d: %q", len(got), tt.want, got)
			}
		})
	}
}
