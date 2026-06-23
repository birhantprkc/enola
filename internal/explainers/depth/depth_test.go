package depth

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// makeStore builds module facts and dependency facts encoding module -> module
// imports. Module names must contain a slash and no dot so they are treated as
// internal by the shared graph builder.
func makeStore(modules []string, deps map[string][]string) *facts.Store {
	s := facts.NewStore()
	for _, m := range modules {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	for src, targets := range deps {
		for _, tgt := range targets {
			s.Add(facts.Fact{
				Kind:      facts.KindDependency,
				File:      src + "/file.go",
				Relations: []facts.Relation{{Kind: facts.RelImports, Target: tgt}},
			})
		}
	}
	return s
}

// chain builds modules a/m0..a/m{n-1} linked m0->m1->...->m{n-1}.
func chain(n int) ([]string, map[string][]string) {
	mods := make([]string, n)
	deps := map[string][]string{}
	for i := 0; i < n; i++ {
		mods[i] = fmt.Sprintf("a/m%d", i)
	}
	for i := 0; i < n-1; i++ {
		deps[mods[i]] = []string{mods[i+1]}
	}
	return mods, deps
}

func TestExplain_ShallowGraph(t *testing.T) {
	mods, deps := chain(4) // longest chain = 4 < minDepth
	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights for shallow graph, got %d", len(insights))
	}
}

func TestExplain_DeepChain(t *testing.T) {
	mods, deps := chain(5) // a/m0 has a chain of length 5
	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 deep-chain insight, got %d: %+v", len(insights), insights)
	}
	in := insights[0]
	if !strings.Contains(in.Title, "a/m0") {
		t.Errorf("deepest module a/m0 should be reported, got title %q", in.Title)
	}
	if len(in.Evidence) != 5 {
		t.Errorf("evidence should list the 5-module chain, got %d", len(in.Evidence))
	}
}

func TestExplain_CycleTerminates(t *testing.T) {
	// a -> b -> c -> a is a cycle; must not infinite-loop, and depth stays small.
	store := makeStore(
		[]string{"a/x", "a/y", "a/z"},
		map[string][]string{
			"a/x": {"a/y"},
			"a/y": {"a/z"},
			"a/z": {"a/x"},
		},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	// Longest cycle-safe chain is 3 (< minDepth), so no insights, and no hang.
	if len(insights) != 0 {
		t.Errorf("expected 0 insights for short cycle, got %d", len(insights))
	}
}

// cyclicGraph: a 3-module cycle x->y->z->x with a tail z->t1->t2->t3. The
// longest simple chain from x visits 6 distinct modules. A correct, cycle-safe
// longest-path must not double-count the cycle entry.
func cyclicGraph() ([]string, map[string][]string) {
	return []string{"a/x", "a/y", "a/z", "a/t1", "a/t2", "a/t3"},
		map[string][]string{
			"a/x":  {"a/y"},
			"a/y":  {"a/z"},
			"a/z":  {"a/x", "a/t1"},
			"a/t1": {"a/t2"},
			"a/t2": {"a/t3"},
		}
}

func TestExplain_CycleDoesNotInflateDepth(t *testing.T) {
	mods, deps := cyclicGraph()
	insights, err := New().Explain(context.Background(), makeStore(mods, deps))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) == 0 {
		t.Fatal("expected at least one deep-chain insight")
	}
	deepest := insights[0]
	// The deepest chain is x->y->z->t1->t2->t3: 6 distinct modules, none repeated.
	if got := len(deepest.Evidence); got != 6 {
		t.Errorf("deepest chain should span 6 distinct modules, got %d: %q", got, deepest.Title)
	}
	seen := map[string]bool{}
	for _, ev := range deepest.Evidence {
		if seen[ev.Fact] {
			t.Errorf("chain double-counts module %q (cycle inflation): %q", ev.Fact, deepest.Title)
		}
		seen[ev.Fact] = true
	}
}

// TestExplain_Deterministic guards against the regression where cycle handling
// depended on Go's randomized map iteration order. Each Explain call re-ranges
// the graph map, so repeated calls exercise different iteration orders; the
// rendered titles must be identical every time.
func TestExplain_Deterministic(t *testing.T) {
	mods, deps := cyclicGraph()
	store := makeStore(mods, deps)

	titles := func() []string {
		insights, err := New().Explain(context.Background(), store)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		out := make([]string, len(insights))
		for i, in := range insights {
			out[i] = in.Title
		}
		return out
	}

	want := strings.Join(titles(), "\n")
	for i := 0; i < 50; i++ {
		if got := strings.Join(titles(), "\n"); got != want {
			t.Fatalf("non-deterministic output on iteration %d:\nwant:\n%s\ngot:\n%s", i, want, got)
		}
	}
}
