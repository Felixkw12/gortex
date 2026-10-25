package store_sqlite

import (
	"bytes"
	"encoding/gob"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/storetest"
)

func TestScopedOutlineParity(t *testing.T) {
	storetest.RunScopedOutline(t, func(t *testing.T) graph.Store {
		s, err := Open(filepath.Join(t.TempDir(), "outline.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestScopedOutlineQueryPlans(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	seedGenerationLookupRows(t, db, 0, 4, 256)
	for _, analyze := range []bool{false, true} {
		if analyze {
			if _, err := db.Exec("ANALYZE"); err != nil {
				t.Fatal(err)
			}
		}
		for _, generation := range []int64{0, 3} {
			scope := graph.NodeScope{WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"repo": true, "other03": true}}
			languages, edges, imports := outlineQueries(scope, generation)
			_, filteredEdges, filteredImports := outlineFilteredQueries(scope, generation, graph.OutlineEdgeFilter{
				ExcludeKinds: []graph.EdgeKind{graph.EdgeMatches}, ExcludeNodeKinds: []graph.NodeKind{graph.KindContract}, ExcludeVia: "spring.Bean",
			})
			viaEdges, viaImports := outlineViaQueries(filteredEdges, filteredImports, "spring.Bean")
			for name, query := range map[string]outlineQuery{"languages": languages, "edges": edges, "imports": imports, "filtered_edges": filteredEdges, "filtered_imports": filteredImports, "via_edges": viaEdges, "via_imports": viaImports} {
				t.Run(name, func(t *testing.T) {
					rows, err := db.Query("EXPLAIN QUERY PLAN "+query.sql, query.args...)
					if err != nil {
						t.Fatal(err)
					}
					defer rows.Close()
					var plan []string
					for rows.Next() {
						var id, parent, unused int
						var detail string
						if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
							t.Fatal(err)
						}
						plan = append(plan, detail)
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					joined := strings.Join(plan, "\n")
					t.Log(joined)
					if !strings.Contains(joined, "nodes_by_repo (repo_prefix=? AND view_gen=?)") {
						t.Fatalf("missing repo seek: %s", joined)
					}
					for _, forbidden := range []string{"SCAN n", "SCAN e", "SCAN target", "SCAN nodes", "SCAN edges"} {
						if strings.Contains(joined, forbidden) {
							t.Fatalf("table scan: %s", joined)
						}
					}
					if strings.HasSuffix(name, "edges") && !strings.Contains(joined, "edges_by_from (view_gen=? AND from_id=?)") {
						t.Fatalf("missing outgoing seek: %s", joined)
					}
					if strings.HasSuffix(name, "imports") && !strings.Contains(joined, "edges_by_to (view_gen=? AND to_id=? AND kind=?)") {
						t.Fatalf("missing incoming seek: %s", joined)
					}
				})
			}
		}
	}
}

func TestScopedOutlineGenerationAndMissingIndexes(t *testing.T) {
	db, _ := openGenerationLookupDB(t)
	for _, row := range []struct {
		id, repo, lang, path string
		generation           int
	}{
		{"from", "a", "go", "a/main.go", 0},
		{"target", "a", "go", "a/lib.go", 0},
		{"from", "outside", "python", "outside/main.py", 3},
		{"target", "a", "rust", "a/lib.rs", 3},
		{"derived-only", "a", "rust", "a/derived.rs", 3},
	} {
		if _, err := db.Exec(`INSERT INTO nodes(id,view_gen,repo_prefix,language,file_path,kind,name,meta) VALUES(?,?,?,?,?,'function','',?)`, row.id, row.generation, row.repo, row.lang, row.path, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		from, to   string
		generation int
	}{
		{"from", "target", 0}, {"from", "target", 3}, {"target", "derived-only", 3},
		// An endpoint existing only in another generation cannot authorize
		// the edge count or attribute imports to that generation's file.
		{"from", "derived-only", 0},
	} {
		if _, err := db.Exec(`INSERT INTO edges(from_id,to_id,kind,view_gen,meta) VALUES(?,?,'imports',?,?)`, row.from, row.to, row.generation, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		for _, gen := range []int64{0, 3} {
			s := &Store{storeCore: &storeCore{db: db}, viewGen: gen}
			got := s.ScopedOutlineCounts(graph.NodeScope{RepoAllow: map[string]bool{"a": true}})
			if got.TotalNodes != 2 || got.TotalEdges != 1 {
				t.Fatalf("generation %d counts: %+v", gen, got)
			}
			if gen == 0 && (got.ByLanguage["go"] != 2 || len(got.FileImports) != 1 || got.FileImports[0].Count != 1 || got.FileImports[0].FilePath != "a/lib.go") {
				t.Fatalf("base counts: %+v", got)
			}
			if gen == 3 && (got.ByLanguage["rust"] != 2 || len(got.FileImports) != 2) {
				t.Fatalf("derived counts: %+v", got)
			}
		}
	}
	check()
	for _, index := range []string{"nodes_by_repo", "edges_by_from", "edges_by_to"} {
		if _, err := db.Exec("DROP INDEX " + index); err != nil {
			t.Fatal(err)
		}
		check()
	}
}

func TestScopedOutlineFilteredParity(t *testing.T) {
	storetest.RunFilteredScopedOutline(t, func(t *testing.T) graph.Store {
		s, err := Open(filepath.Join(t.TempDir(), "outline.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestScopedOutlineMetadataEncodings(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/a.go"},
		{ID: "b", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/b.go"},
	}, nil)
	flat, err := encodeMeta(map[string]any{"via": "spring.Bean"})
	if err != nil {
		t.Fatal(err)
	}
	falsePositive, err := encodeMeta(map[string]any{"other": "spring.Bean", "via": "spring.BeanFactory"})
	if err != nil {
		t.Fatal(err)
	}
	var legacy bytes.Buffer
	if err := gob.NewEncoder(&legacy).Encode(map[string]any{"via": "spring.Bean"}); err != nil {
		t.Fatal(err)
	}
	for i, blob := range [][]byte{flat, []byte(`{"via":"spring.Bean"}`), []byte(`{"via":"\u0073pring.Bean"}`), legacy.Bytes(), falsePositive, nil} {
		for _, kind := range []string{"calls", "imports"} {
			if _, err := s.writerDB.Exec(`INSERT INTO edges(from_id,to_id,kind,line,view_gen,meta) VALUES('a','b',?,?,0,?)`, kind, i, blob); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := s.ScopedOutlineCountsFiltered(graph.NodeScope{RepoAllow: map[string]bool{"repo": true}}, graph.OutlineEdgeFilter{ExcludeVia: "spring.Bean"})
	if got.TotalEdges != 4 || len(got.FileImports) != 1 || got.FileImports[0].Count != 2 {
		t.Fatalf("mixed metadata counts = %+v, want 4 edges and 2 imports", got)
	}
}
