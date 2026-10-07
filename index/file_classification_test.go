package index

import (
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

func TestDisableFileClassification(t *testing.T) {
	docs := []Document{
		{Name: "hello.h", Content: []byte("#include <stdio.h>")},
		{Name: "foo_test.go", Content: []byte("package foo")},
		{Name: "explicit.txt", Language: "Go", Content: []byte("package explicit")},
	}
	build := func(disable bool) *ShardBuilder {
		b, err := NewShardBuilder(&zoekt.Repository{Name: "repo"})
		if err != nil {
			t.Fatal(err)
		}
		b.disableFileClassification = disable
		for _, d := range docs {
			if err := b.Add(d); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}

	classified := build(false)
	if res := searchForTest(t, classified, &query.Language{Language: "C"}); len(res.Files) != 1 {
		t.Fatalf("classified: got %d C files, want 1", len(res.Files))
	}
	testCategory, _ := FileCategoryTest.encode()
	if classified.categories[1] != testCategory {
		t.Fatalf("classified: foo_test.go category %v", classified.categories[1])
	}

	plain := build(true)
	if res := searchForTest(t, plain, &query.Language{Language: "C"}); len(res.Files) != 0 {
		t.Fatalf("unclassified: got %d C files, want 0", len(res.Files))
	}
	if res := searchForTest(t, plain, &query.Language{Language: "Go"}); len(res.Files) != 1 || res.Files[0].FileName != "explicit.txt" {
		t.Fatalf("unclassified: explicit language lost: %+v", res.Files)
	}
	defaultCategory, _ := FileCategoryDefault.encode()
	for i, c := range plain.categories {
		if c != defaultCategory {
			t.Fatalf("unclassified: document %d category %v, want default", i, c)
		}
	}
}

func TestSortDocumentsClassified(t *testing.T) {
	docs := func() []*Document {
		return []*Document{{Name: "vendor/a.go"}, {Name: "b.go"}}
	}
	got := docs()
	sortDocumentsClassified(got, true)
	if got[0].Name != "b.go" {
		t.Fatalf("classified order %s, %s", got[0].Name, got[1].Name)
	}
	got = docs()
	sortDocumentsClassified(got, false)
	if got[0].Name != "b.go" {
		// Without classification the shorter name still sorts first.
		t.Fatalf("unclassified order %s, %s", got[0].Name, got[1].Name)
	}
	got = []*Document{{Name: "vendor/a"}, {Name: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.go"}}
	sortDocumentsClassified(got, false)
	if got[0].Name != "vendor/a" {
		t.Fatalf("unclassified order must ignore vendoring: %s, %s", got[0].Name, got[1].Name)
	}
}
