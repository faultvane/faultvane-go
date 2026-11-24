package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `package sample

func Classify(n int) string {
	if n < 0 {
		return "negative" + "!"
	}
	if n == 0 || n > 100 {
		return "edge"
	}
	return "normal"
}

func Sum(a, b int) int {
	return a + b
}
`

func TestDiscover(t *testing.T) {
	mutants, err := discover("sample.go", []byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range mutants {
		got = append(got, m.String())
	}
	want := []string{
		"sample.go:4:7 < -> <=",
		"sample.go:7:12 || -> &&",
		"sample.go:7:7 == -> !=",
		"sample.go:7:17 > -> >=",
		"sample.go:14:11 + -> -",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("mutants:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if changed := string(mutants[0].apply([]byte(sample))); !strings.Contains(changed, "if n <= 0 {") || strings.Count(changed, "\n") != strings.Count(sample, "\n") {
		t.Fatalf("mutated source:\n%s", changed)
	}
	if _, err := discover("broken.go", []byte("package")); err == nil {
		t.Fatal("discover() of a file that does not parse returned no error")
	}
}

func module(t *testing.T, test string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":                "module sample\n\ngo 1.24\n",
		"sample/sample.go":      sample,
		"sample/sample_test.go": test,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const strongTest = `package sample

import "testing"

func TestAll(t *testing.T) {
	cases := map[int]string{-1: "negative!", 0: "edge", 1: "normal", 100: "normal", 101: "edge"}
	for n, want := range cases {
		if got := Classify(n); got != want {
			t.Fatalf("Classify(%d) = %q, want %q", n, got, want)
		}
	}
	if Sum(2, 3) != 5 {
		t.Fatal("Sum")
	}
}
`

const weakTest = `package sample

import "testing"

func TestSome(t *testing.T) {
	if Classify(50) != "normal" || Sum(2, 0) != 2 {
		t.Fatal("wrong")
	}
}
`

func TestRun(t *testing.T) {
	t.Run("QA-9 strong tests kill each mutant", func(t *testing.T) {
		var out strings.Builder
		status := run([]string{"-dir", module(t, strongTest), "-workers", "2", "sample"}, &out)
		if status != 0 || !strings.Contains(out.String(), "score 100.0%  killed 5  survived 0  not compiled 0  ok") {
			t.Fatalf("status %d, output:\n%s", status, out.String())
		}
	})

	t.Run("QA-9 weak tests give survivors and a failed gate", func(t *testing.T) {
		var out strings.Builder
		status := run([]string{"-dir", module(t, weakTest), "-min", "90", "sample"}, &out)
		output := out.String()
		if status != 1 || !strings.Contains(output, "BELOW MINIMUM") || !strings.Contains(output, "survived: sample/sample.go:4:7 < -> <=") || !strings.Contains(output, "survived: sample/sample.go:14:11 + -> -") {
			t.Fatalf("status %d, output:\n%s", status, output)
		}
	})

	t.Run("QA-9 the minimum is configurable", func(t *testing.T) {
		var out strings.Builder
		if status := run([]string{"-dir", module(t, weakTest), "-min", "10", "sample"}, &out); status != 0 {
			t.Fatalf("status %d, output:\n%s", status, out.String())
		}
	})

	t.Run("QA-9 wrong arguments give the usage text", func(t *testing.T) {
		for _, arguments := range [][]string{{}, {"-workers", "0", "sample"}, {"-unknown"}, {"-dir", t.TempDir(), "missing"}} {
			var out strings.Builder
			if status := run(arguments, &out); status != 2 || out.Len() == 0 {
				t.Fatalf("arguments %v: status %d, output %q", arguments, status, out.String())
			}
		}
	})
}
