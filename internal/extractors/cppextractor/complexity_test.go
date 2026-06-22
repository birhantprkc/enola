package cppextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers ---

func intProp(t *testing.T, f facts.Fact, key string) int {
	t.Helper()
	v, ok := f.Props[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case float64: // after a JSONL round-trip ints decode as float64
		return int(n)
	}
	t.Fatalf("prop %q is not numeric: %T", key, v)
	return 0
}

func strSliceProp(f facts.Fact, key string) []string {
	v, ok := f.Props[key]
	if !ok {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, a := range s {
			if str, ok := a.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func containsStr(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

// --- tests ---

func TestCppComplexity_NestedLoops(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"pkg/proc.cpp": `
void helper(int x);

void process(int* items, int n) {
	for (int i = 0; i < n; i++) {
		for (int j = 0; j < items[i]; j++) {
			helper(j);
		}
	}
}
`,
	})

	f, ok := findFact(ff, "pkg.process")
	if !ok {
		t.Fatalf("missing pkg.process; got %v", factNames(ff))
	}
	if got := intProp(t, f, "loop_depth"); got != 2 {
		t.Errorf("loop_depth = %d, want 2", got)
	}
	if got := intProp(t, f, "loop_count"); got != 2 {
		t.Errorf("loop_count = %d, want 2", got)
	}
	// 1 (base) + 2 loops = 3.
	if got := intProp(t, f, "cyclomatic"); got != 3 {
		t.Errorf("cyclomatic = %d, want 3", got)
	}
	if cil := strSliceProp(f, "calls_in_loop"); !containsStr(cil, "pkg.helper") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.helper", cil)
	}
}

func TestCppComplexity_RangeFor(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"pkg/r.cpp": `
#include <vector>
void consume(int x);

void run(const std::vector<int>& items) {
	for (auto& x : items) {
		consume(x);
	}
}
`,
	})
	f, ok := findFact(ff, "pkg.run")
	if !ok {
		t.Fatalf("missing pkg.run; got %v", factNames(ff))
	}
	if got := intProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1", got)
	}
	if got := intProp(t, f, "loop_count"); got != 1 {
		t.Errorf("loop_count = %d, want 1", got)
	}
}

func TestCppComplexity_CallsInLoop_InVsOutside(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"pkg/mixed.cpp": `
void setup();
void inLoop(int x);

void mixed(int n) {
	setup();
	for (int i = 0; i < n; i++) {
		inLoop(i);
	}
}
`,
	})
	f, ok := findFact(ff, "pkg.mixed")
	if !ok {
		t.Fatalf("missing pkg.mixed; got %v", factNames(ff))
	}
	cil := strSliceProp(f, "calls_in_loop")
	if !containsStr(cil, "pkg.inLoop") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.inLoop", cil)
	}
	if containsStr(cil, "pkg.setup") {
		t.Errorf("calls_in_loop = %v, must NOT contain pkg.setup (called outside loop)", cil)
	}
}

func TestCppComplexity_Recursion(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"pkg/fib.cpp": `
int fib(int n) {
	if (n < 2) {
		return n;
	}
	return fib(n - 1) + fib(n - 2);
}
`,
	})
	f, ok := findFact(ff, "pkg.fib")
	if !ok {
		t.Fatalf("missing pkg.fib; got %v", factNames(ff))
	}
	if f.Props["recursive_self"] != true {
		t.Errorf("fib should be recursive_self, got props %+v", f.Props)
	}
}

func TestCppComplexity_StlAlgorithmLambda(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"pkg/stl.cpp": `
#include <vector>
#include <algorithm>
void consume(int x);

void run(const std::vector<int>& v) {
	std::for_each(v.begin(), v.end(), [&](int x) {
		consume(x);
	});
}
`,
	})
	f, ok := findFact(ff, "pkg.run")
	if !ok {
		t.Fatalf("missing pkg.run; got %v", factNames(ff))
	}
	if got := intProp(t, f, "loop_count"); got < 1 {
		t.Errorf("loop_count = %d, want >= 1 (std::for_each lambda is a loop)", got)
	}
	if got := intProp(t, f, "loop_depth"); got < 1 {
		t.Errorf("loop_depth = %d, want >= 1", got)
	}
	if cil := strSliceProp(f, "calls_in_loop"); !containsStr(cil, "pkg.consume") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.consume", cil)
	}
}

func TestCppComplexity_DeferredLambdaResetsDepth(t *testing.T) {
	// A plain lambda defined inside a loop but stored (not invoked per-iteration) is a
	// deferred scope: calls inside it must NOT be charged as in-loop.
	ff := extractProject(t, map[string]string{
		"pkg/defer.cpp": `
#include <functional>
void work();

void run(int n) {
	for (int i = 0; i < n; i++) {
		std::function<void()> cb = [&]() {
			work();
		};
	}
}
`,
	})
	f, ok := findFact(ff, "pkg.run")
	if !ok {
		t.Fatalf("missing pkg.run; got %v", factNames(ff))
	}
	// The syntactic for loop still counts.
	if got := intProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1 (the for loop; lambda body is deferred)", got)
	}
	if cil := strSliceProp(f, "calls_in_loop"); containsStr(cil, "pkg.work") {
		t.Errorf("calls_in_loop = %v, must NOT contain pkg.work (deferred lambda body)", cil)
	}
}

func TestCppComplexity_OutOfLineMethod(t *testing.T) {
	// A method declared in the header and defined out-of-line in the .cpp carries the
	// metrics from its definition after dedup.
	ff := extractProject(t, map[string]string{
		"pkg/widget.hpp": `
class Widget {
public:
	void refresh(int n);
};
`,
		"pkg/widget.cpp": `
#include "widget.hpp"
void tick();

void Widget::refresh(int n) {
	for (int i = 0; i < n; i++) {
		tick();
	}
}
`,
	})
	f, ok := findFact(ff, "pkg.Widget::refresh")
	if !ok {
		t.Fatalf("missing pkg.Widget::refresh; got %v", factNames(ff))
	}
	if n := countByName(ff, "pkg.Widget::refresh"); n != 1 {
		t.Errorf("expected exactly 1 Widget::refresh fact (dedup), got %d", n)
	}
	if got := intProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1", got)
	}
}

func TestCppComplexity_InlineMethodRecursion(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"pkg/tree.cpp": `
struct Node {
	int sum() {
		int total = value;
		for (Node* c : children) {
			total += c->sum();
		}
		return total;
	}
	int value;
};
`,
	})
	f, ok := findFact(ff, "pkg.Node::sum")
	if !ok {
		t.Fatalf("missing pkg.Node::sum; got %v", factNames(ff))
	}
	if got := intProp(t, f, "loop_depth"); got != 1 {
		t.Errorf("loop_depth = %d, want 1", got)
	}
	// c->sum() is a non-this receiver call; recorded for calls_in_loop.
	if cil := strSliceProp(f, "calls_in_loop"); !containsStr(cil, "c.sum") {
		t.Errorf("calls_in_loop = %v, want to contain c.sum", cil)
	}
}

func TestCppComplexity_CheapContainerMethodsNotRecorded(t *testing.T) {
	// A loop over a container's iterators must not record begin()/end()/size() as
	// per-iteration work — those are cheap accessors, not N+1 I/O. This guards the
	// false positive where a JSON `db.begin()` matched the generic `db.` I/O prefix.
	ff := extractProject(t, map[string]string{
		"pkg/iter.cpp": `
void heavy(int x);

void run(const Json& db) {
	for (auto it = db.begin(); it != db.end(); ++it) {
		if (db.size() > 0) {
			heavy(*it);
		}
	}
}
`,
	})
	f, ok := findFact(ff, "pkg.run")
	if !ok {
		t.Fatalf("missing pkg.run; got %v", factNames(ff))
	}
	cil := strSliceProp(f, "calls_in_loop")
	for _, cheap := range []string{"db.begin", "db.end", "db.size"} {
		if containsStr(cil, cheap) {
			t.Errorf("calls_in_loop = %v, must NOT contain cheap accessor %q", cil, cheap)
		}
	}
	// Real per-iteration work is still recorded.
	if !containsStr(cil, "pkg.heavy") {
		t.Errorf("calls_in_loop = %v, want to contain pkg.heavy", cil)
	}
}

func TestCppComplexity_CyclomaticDecisions(t *testing.T) {
	ff := extractProject(t, map[string]string{
		"pkg/dec.cpp": `
int classify(int a, int b) {
	if (a > 0 && b > 0) {
		return 1;
	}
	int r = a > b ? a : b;
	switch (r) {
	case 1:
		return 10;
	case 2:
		return 20;
	}
	return 0;
}
`,
	})
	f, ok := findFact(ff, "pkg.classify")
	if !ok {
		t.Fatalf("missing pkg.classify; got %v", factNames(ff))
	}
	// 1 (base) + if + && + ternary + 2 cases = 6.
	if got := intProp(t, f, "cyclomatic"); got != 6 {
		t.Errorf("cyclomatic = %d, want 6", got)
	}
}
