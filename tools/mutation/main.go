package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var replacements = map[token.Token]token.Token{
	token.LSS:  token.LEQ,
	token.LEQ:  token.LSS,
	token.GTR:  token.GEQ,
	token.GEQ:  token.GTR,
	token.EQL:  token.NEQ,
	token.NEQ:  token.EQL,
	token.LAND: token.LOR,
	token.LOR:  token.LAND,
	token.ADD:  token.SUB,
	token.SUB:  token.ADD,
}

type mutant struct {
	file   string
	offset int
	line   int
	column int
	from   string
	to     string
}

func (m mutant) String() string {
	return fmt.Sprintf("%s:%d:%d %s -> %s", m.file, m.line, m.column, m.from, m.to)
}

func (m mutant) apply(source []byte) []byte {
	mutated := make([]byte, 0, len(source)+1)
	mutated = append(mutated, source[:m.offset]...)
	mutated = append(mutated, m.to...)
	return append(mutated, source[m.offset+len(m.from):]...)
}

func isText(expression ast.Expr) bool {
	literal, ok := expression.(*ast.BasicLit)
	return ok && literal.Kind == token.STRING
}

func discover(file string, source []byte) ([]mutant, error) {
	set := token.NewFileSet()
	parsed, err := parser.ParseFile(set, file, source, 0)
	if err != nil {
		return nil, err
	}
	var mutants []mutant
	ast.Inspect(parsed, func(node ast.Node) bool {
		expression, ok := node.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		to, known := replacements[expression.Op]
		if !known || isText(expression.X) || isText(expression.Y) {
			return true
		}
		position := set.Position(expression.OpPos)
		mutants = append(mutants, mutant{file: file, offset: position.Offset, line: position.Line, column: position.Column, from: expression.Op.String(), to: to.String()})
		return true
	})
	return mutants, nil
}

type outcome int

const (
	killed outcome = iota
	survived
	invalid
)

type runner struct {
	root    string
	timeout time.Duration
	scratch string
}

func (r runner) test(index int, m mutant, pattern string) (outcome, error) {
	original := filepath.Join(r.root, m.file)
	source, err := os.ReadFile(original)
	if err != nil {
		return invalid, err
	}
	changed := filepath.Join(r.scratch, fmt.Sprintf("mutant-%d.go", index))
	if err := os.WriteFile(changed, m.apply(source), 0o600); err != nil {
		return invalid, err
	}
	overlay := filepath.Join(r.scratch, fmt.Sprintf("overlay-%d.json", index))
	content, _ := json.Marshal(map[string]map[string]string{"Replace": {original: changed}})
	if err := os.WriteFile(overlay, content, 0o600); err != nil {
		return invalid, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-count=1", "-overlay", overlay, pattern)
	command.Dir = r.root
	output, err := command.CombinedOutput()
	switch {
	case err == nil:
		return survived, nil
	case ctx.Err() != nil:
		return killed, nil
	case strings.Contains(string(output), "[build failed]") || strings.Contains(string(output), "[setup failed]"):
		return invalid, nil
	}
	return killed, nil
}

type report struct {
	pkg       string
	killed    int
	invalid   int
	survivors []mutant
}

func (r report) score() float64 {
	total := r.killed + len(r.survivors)
	if total == 0 {
		return 100
	}
	return 100 * float64(r.killed) / float64(total)
}

func mutantsOf(root, pkg string) ([]mutant, error) {
	entries, err := os.ReadDir(filepath.Join(root, pkg))
	if err != nil {
		return nil, err
	}
	var all []mutant
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file := filepath.Join(pkg, name)
		source, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			return nil, err
		}
		found, err := discover(file, source)
		if err != nil {
			return nil, err
		}
		all = append(all, found...)
	}
	return all, nil
}

func examine(r runner, pkg string, workers int) (report, error) {
	mutants, err := mutantsOf(r.root, pkg)
	if err != nil {
		return report{}, err
	}
	result := report{pkg: pkg}
	var mu sync.Mutex
	var group sync.WaitGroup
	var failure error
	jobs := make(chan int)
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				verdict, err := r.test(index, mutants[index], "./"+filepath.ToSlash(pkg)+"/...")
				mu.Lock()
				switch {
				case err != nil:
					failure = err
				case verdict == killed:
					result.killed++
				case verdict == survived:
					result.survivors = append(result.survivors, mutants[index])
				default:
					result.invalid++
				}
				mu.Unlock()
			}
		}()
	}
	for index := range mutants {
		jobs <- index
	}
	close(jobs)
	group.Wait()
	sort.Slice(result.survivors, func(i, j int) bool { return result.survivors[i].String() < result.survivors[j].String() })
	return result, failure
}

func run(arguments []string, out io.Writer) int {
	flags := flag.NewFlagSet("mutation", flag.ContinueOnError)
	flags.SetOutput(out)
	root := flags.String("dir", ".", "the module directory")
	minimum := flags.Float64("min", 90, "the minimum mutation score of each package, in percent")
	workers := flags.Int("workers", 4, "the number of test runs at the same time")
	timeout := flags.Duration("timeout", 3*time.Minute, "the time limit of one test run")
	if err := flags.Parse(arguments); err != nil || flags.NArg() == 0 || *workers < 1 {
		fmt.Fprintln(out, "usage: mutation [-dir module] [-min percent] [-workers n] [-timeout duration] package...")
		return 2
	}
	absolute, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(out, err)
		return 2
	}
	scratch, err := os.MkdirTemp("", "faultvane-mutation")
	if err != nil {
		fmt.Fprintln(out, err)
		return 2
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	r := runner{root: absolute, timeout: *timeout, scratch: scratch}
	status := 0
	for _, pkg := range flags.Args() {
		result, err := examine(r, pkg, *workers)
		if err != nil {
			fmt.Fprintln(out, err)
			return 2
		}
		verdict := "ok"
		if result.score() < *minimum {
			verdict = "BELOW MINIMUM"
			status = 1
		}
		fmt.Fprintf(out, "%-24s score %5.1f%%  killed %d  survived %d  not compiled %d  %s\n", result.pkg, result.score(), result.killed, len(result.survivors), result.invalid, verdict)
		for _, survivor := range result.survivors {
			fmt.Fprintf(out, "  survived: %s\n", survivor)
		}
	}
	return status
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}
