package graph

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
)

type declarationVisitorStub struct {
	*exactNamesVisitorStub
	declarations int
}

func (r *declarationVisitorStub) VisitNodeDeclarationsByNamesContext(ctx context.Context, names []string, yield func(*Node) bool) error {
	r.declarations++
	for _, n := range r.nodes {
		if !yield(n) {
			return ctx.Err()
		}
	}
	return r.err
}

// Unwrap must not let the optimized reader bypass this wrapper's error.
type guardedDeclarationReader struct {
	Reader
	cause error
}

func (r *guardedDeclarationReader) Unwrap() Reader { return r.Reader }
func (r *guardedDeclarationReader) VisitNodesByNamesContext(context.Context, []string, func(*Node) bool) error {
	return r.cause
}
func TestExactNameDeclarationsOverlayMasksAndCheckedFallback(t *testing.T) {
	layer := NewOverlayLayer()
	layer.MarkFile("covered.go", false)
	layer.MarkFile("deleted.go", true)
	layer.MarkRemoved("Old", "removed")
	layer.AddNode("covered.go", &Node{ID: "covered.go::new", Name: "New", FilePath: "covered.go"})
	layer.AddNode("", &Node{ID: "identity", Name: "New", FilePath: "outside.go"})
	base := &declarationVisitorStub{exactNamesVisitorStub: &exactNamesVisitorStub{nodes: []*Node{
		{ID: "covered.go::old", Name: "Old"}, {ID: "deleted.go::old", Name: "Old"}, {ID: "identity", Name: "Old"}, {ID: "removed", Name: "Old"}, {ID: "visible.go::old", Name: "Old"},
	}}}
	view := NewOverlaidViewWithLayer(base, layer)
	var got []string
	if err := VisitNodeDeclarationsByNamesContext(t.Context(), view, []string{"New", "Old"}, func(n *Node) bool { got = append(got, n.ID); return true }); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"covered.go::new", "identity", "visible.go::old"}) || base.declarations != 1 || base.calls != 0 {
		t.Fatalf("visible=%v declarations=%d full=%d", got, base.declarations, base.calls)
	}
	base.declarations = 0
	if err := VisitNodeDeclarationsByNamesContext(t.Context(), view, []string{"New", "Old"}, func(*Node) bool { return false }); err != nil || base.declarations != 0 {
		t.Fatalf("stop=%v calls=%d", err, base.declarations)
	}
	cause := errors.New("guarded incomplete source")
	if err := VisitNodeDeclarationsByNamesContext(t.Context(), &guardedDeclarationReader{Reader: base, cause: cause}, []string{"New", "Old"}, func(*Node) bool { t.Fatal("guard bypass"); return true }); !errors.Is(err, cause) {
		t.Fatalf("guard=%v", err)
	}
	broken := &contextualBatchFallbackLayer{OverlayLayer: NewOverlayLayer(), err: cause}
	if err := VisitNodeDeclarationsByNamesContext(t.Context(), NewOverlaidViewWithLayer(base, broken), []string{"A", "B"}, func(*Node) bool { t.Fatal("partial source blessed"); return true }); !errors.Is(err, cause) {
		t.Fatalf("contextual layer=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	if err := VisitNodeDeclarationsByNamesContext(ctx, base, []string{"A", "B"}, func(*Node) bool { cancel(); return true }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type guardedRepoDeclarationReader struct {
	*declarationVisitorStub
	cause error
}

func (r *guardedRepoDeclarationReader) VisitNodesByNamesInRepoContext(context.Context, []string, string, func(*Node) bool) error {
	return r.cause
}
func TestExactNameDeclarationsPreserveRepoWrapperRefusal(t *testing.T) {
	cause := errors.New("repo source unavailable")
	r := &guardedRepoDeclarationReader{declarationVisitorStub: &declarationVisitorStub{exactNamesVisitorStub: &exactNamesVisitorStub{}}, cause: cause}
	err := VisitNodeDeclarationsByNamesInRepoContext(t.Context(), r, []string{"A", "B"}, "repo", func(*Node) bool { t.Fatal("repo source bypassed"); return true })
	if !errors.Is(err, cause) || r.declarations != 0 {
		t.Fatalf("repo guard=%v optimizedcalls=%d", err, r.declarations)
	}
}
