package bitio

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mustInline lists the functions whose speed depends on being inlined rather than
// called. The reader's are called once per coefficient pair and the writer's once
// per field, so a call left in any of them is a few percent of a repack; NewReader
// is here for a different reason, that a Reader which cannot be built inline
// escapes to the heap, several times per frame.
//
// None of them fits the inliner by accident. Peek64 reads its zero-filled tail
// from a padded copy rather than calling for it precisely because a call the
// inliner cannot see through costs 57 of its budget of 80, and Read only fits
// because Peek64 does. That accounting is an unversioned implementation detail of
// cmd/compile: it can move under us in a release, silently, with every other test
// still passing. Hence this one.
var mustInline = []string{
	"(*Reader).PeekAt",
	"(*Reader).Peek64",
	"(*Reader).Read",
	"(*Reader).Skip",
	"(*Reader).Tell",
	"NewReader",
	"(*Writer).Write",
	"(*Writer).Tell",
}

func TestHotPathsStillInline(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool in PATH to ask about inlining")
	}

	// The compiler prints its decisions while compiling, and a cached build does
	// not compile. Keying the cache on a module path unique to this run is what
	// makes the answer come from the compiler rather than from the cache; copying
	// the sources is only how that path is arrived at. The package imports nothing
	// outside the standard library, so nothing has to be fetched or vendored.
	dir := t.TempDir()
	srcs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	copied := 0
	for _, src := range srcs {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, src), b, 0o644); err != nil {
			t.Fatal(err)
		}
		copied++
	}
	if copied == 0 {
		t.Fatal("no package sources found to compile")
	}

	mod := "inlinecheck" + sanitize(filepath.Base(dir))
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module "+mod+"\n\n"+goDirective(t)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(goTool, "build", "-gcflags=-m", ".")
	cmd.Dir = dir
	// The parent workspace and any GOFLAGS in the environment would apply to this
	// build too, and neither has anything to say about it.
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOPROXY=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build -gcflags=-m: %v\n%s", err, out)
	}
	report := string(out)

	// Without this the check could pass by seeing nothing at all, which is the one
	// way it must not pass: a cached build, a flag that stopped meaning what it
	// meant, a toolchain that reports elsewhere.
	if !strings.Contains(report, "can inline") {
		t.Fatalf("the compiler reported no inlining decisions, so this test cannot see what it guards:\n%s", report)
	}

	for _, fn := range mustInline {
		if !strings.Contains(report, "can inline "+fn) {
			t.Errorf("%s is no longer inlined; it is called once per pair or per field, so this is worth a few percent of a repack.\n"+
				"Run `go build -gcflags='-m -m' ./internal/bitio` for the cost against the budget.", fn)
		}
	}
}

// goDirective returns the module's own go directive, so that the check compiles
// against the same language version the package is built with.
func goDirective(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^go .*$`).Find(b)
	if m == nil {
		t.Fatal("no go directive in go.mod")
	}
	return string(m)
}

// sanitize reduces a temporary directory's name to something a module path allows.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		}
		return -1
	}, s)
}
