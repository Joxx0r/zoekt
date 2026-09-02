package index

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/internal/testutil"
	"github.com/sourcegraph/zoekt/query"
)

func TestSearchWithExactCountSourceLineIdentity(t *testing.T) {
	searcher := exactCountSearcherForTest(t,
		Document{Name: "a.go", Content: []byte("needle needle\nnone\nneedle\n")},
		Document{Name: "b.go", Content: []byte("needle\n")},
		Document{Name: "c.go", Content: []byte("none\n")},
	)
	q := &query.Or{Children: []query.Q{
		&query.Substring{Pattern: "needle"},
		&query.Substring{Pattern: "needle"},
	}}
	opts := &zoekt.SearchOptions{
		ShardMaxMatchCount:   1,
		TotalMaxMatchCount:   1,
		MaxDocDisplayCount:   1,
		MaxMatchDisplayCount: 1,
	}

	result, counts, err := searcher.SearchWithExactCount(context.Background(), context.Background(), q, opts)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(&zoekt.ExactSearchCounts{MatchCount: 3, FileCount: 2}, counts); diff != "" {
		t.Fatalf("exact counts mismatch (-want +got):\n%s", diff)
	}
	if got := resultLineCount(result); got != 1 {
		t.Fatalf("bounded result lines = %d, want 1", got)
	}

	legacy, err := searcher.Search(context.Background(), q, opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Files = SortAndTruncateFiles(legacy.Files, opts)
	if diff := cmp.Diff(legacy.Files, result.Files); diff != "" {
		t.Fatalf("bounded result changed (-legacy +counted):\n%s", diff)
	}
}

func TestSearchWithExactCountCountsLinesForChunkResults(t *testing.T) {
	searcher := exactCountSearcherForTest(t,
		Document{Name: "multiline.go", Content: []byte("first needle\nsecond needle\n")},
	)
	opts := &zoekt.SearchOptions{
		ChunkMatches:         true,
		ShardMaxMatchCount:   1,
		MaxDocDisplayCount:   1,
		MaxMatchDisplayCount: 1,
	}

	result, counts, err := searcher.SearchWithExactCount(
		context.Background(), context.Background(), &query.Substring{Pattern: "needle"}, opts,
	)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(&zoekt.ExactSearchCounts{MatchCount: 2, FileCount: 1}, counts); diff != "" {
		t.Fatalf("exact counts mismatch (-want +got):\n%s", diff)
	}
	if len(result.Files) != 1 || len(result.Files[0].ChunkMatches) != 1 {
		t.Fatalf("bounded chunk result = %#v, want one file and one chunk", result.Files)
	}
}

func TestSearchWithExactCountZeroAndFilenameRows(t *testing.T) {
	searcher := exactCountSearcherForTest(t,
		Document{Name: "needle.go", Content: []byte("haystack\n")},
	)
	opts := &zoekt.SearchOptions{MaxDocDisplayCount: 1, MaxMatchDisplayCount: 1}

	tests := []struct {
		name string
		q    query.Q
		want zoekt.ExactSearchCounts
	}{
		{
			name: "zero",
			q:    &query.Substring{Pattern: "absent"},
			want: zoekt.ExactSearchCounts{},
		},
		{
			name: "filename only",
			q:    &query.Substring{Pattern: "needle", FileName: true},
			want: zoekt.ExactSearchCounts{MatchCount: 1, FileCount: 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, counts, err := searcher.SearchWithExactCount(context.Background(), context.Background(), test.q, opts)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(&test.want, counts); diff != "" {
				t.Fatalf("exact counts mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSearchWithExactCountIgnoresEstimateOnlyShortcut(t *testing.T) {
	searcher := exactCountSearcherForTest(t,
		Document{Name: "a.go", Content: []byte("needle\nneedle\n")},
	)
	opts := &zoekt.SearchOptions{
		EstimateDocCount:     true,
		MaxDocDisplayCount:   1,
		MaxMatchDisplayCount: 1,
	}

	result, counts, err := searcher.SearchWithExactCount(
		context.Background(), context.Background(), &query.Substring{Pattern: "needle"}, opts,
	)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(&zoekt.ExactSearchCounts{MatchCount: 2, FileCount: 1}, counts); diff != "" {
		t.Fatalf("exact counts mismatch (-want +got):\n%s", diff)
	}
	if len(result.Files) != 1 {
		t.Fatalf("bounded result files = %d, want 1", len(result.Files))
	}
}

func TestCountSourceLinesIncludesZeroWidthAndNewlineOnlyMatches(t *testing.T) {
	builder := testShardBuilder(t, nil,
		Document{Name: "a.go", Content: []byte("first\nsecond\n")},
	)
	searcher := searcherForTest(t, builder)
	d, ok := searcher.(*indexData)
	if !ok {
		t.Fatalf("searcher type = %T, want *indexData", searcher)
	}
	cp := &contentProvider{id: d, stats: &zoekt.Stats{}}
	cp.setDocument(0)

	tests := []struct {
		name    string
		matches []*candidateMatch
		want    int
	}{
		{
			name:    "zero width",
			matches: []*candidateMatch{{byteOffset: 0}},
			want:    1,
		},
		{
			name:    "newline only",
			matches: []*candidateMatch{{byteOffset: 5, byteMatchSz: 1}},
			want:    1,
		},
		{
			name:    "newline and following text",
			matches: []*candidateMatch{{byteOffset: 5, byteMatchSz: 7}},
			want:    2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, complete, err := countSourceLines(context.Background(), context.Background(), cp, test.matches)
			if err != nil {
				t.Fatal(err)
			}
			if !complete {
				t.Fatal("count unexpectedly incomplete")
			}
			if got != test.want {
				t.Fatalf("source line count = %d, want %d", got, test.want)
			}
		})
	}
}

func TestCountSourceLinesChecksContextsForZeroWidthMatches(t *testing.T) {
	builder := testShardBuilder(t, nil,
		Document{Name: "a.go", Content: []byte("first\nsecond\n")},
	)
	searcher := searcherForTest(t, builder)
	d, ok := searcher.(*indexData)
	if !ok {
		t.Fatalf("searcher type = %T, want *indexData", searcher)
	}
	cp := &contentProvider{id: d, stats: &zoekt.Stats{}}
	cp.setDocument(0)
	matches := []*candidateMatch{{byteOffset: 0}}

	t.Run("request cancellation", func(t *testing.T) {
		got, complete, err := countSourceLines(testutil.NewErrAfterContext(1), context.Background(), cp, matches)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want DeadlineExceeded", err)
		}
		if complete || got != 0 {
			t.Fatalf("count = %d, complete = %t; want 0, false", got, complete)
		}
	})

	t.Run("count budget expiry", func(t *testing.T) {
		got, complete, err := countSourceLines(context.Background(), testutil.NewErrAfterContext(1), cp, matches)
		if err != nil {
			t.Fatal(err)
		}
		if complete || got != 0 {
			t.Fatalf("count = %d, complete = %t; want 0, false", got, complete)
		}
	})
}

func TestSearchWithExactCountCountsNewlineOnlySourceLines(t *testing.T) {
	searcher := exactCountSearcherForTest(t,
		Document{Name: "multiline.go", Content: []byte("first\nsecond\n")},
		Document{Name: "single.go", Content: []byte("no newline")},
	)
	opts := &zoekt.SearchOptions{MaxDocDisplayCount: 1, MaxMatchDisplayCount: 1}

	_, counts, err := searcher.SearchWithExactCount(
		context.Background(), context.Background(), &query.Substring{Pattern: "\n"}, opts,
	)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(&zoekt.ExactSearchCounts{MatchCount: 2, FileCount: 1}, counts); diff != "" {
		t.Fatalf("newline-only counts mismatch (-want +got):\n%s", diff)
	}
}

func TestSearchWithExactCountBudgetExpiryPreservesBoundedResult(t *testing.T) {
	docs := make([]Document, 10)
	for i := range docs {
		docs[i] = Document{Name: fmt.Sprintf("file-%02d.go", i), Content: []byte("needle\n")}
	}
	searcher := exactCountSearcherForTest(t, docs...)
	q := &query.Substring{Pattern: "needle"}
	opts := &zoekt.SearchOptions{
		ShardMaxMatchCount:   2,
		TotalMaxMatchCount:   2,
		MaxDocDisplayCount:   2,
		MaxMatchDisplayCount: 2,
	}
	legacy, err := searcher.Search(context.Background(), q, opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Files = SortAndTruncateFiles(legacy.Files, opts)

	t.Run("before display window", func(t *testing.T) {
		countCtx, cancel := context.WithCancel(context.Background())
		cancel()
		result, counts, err := searcher.SearchWithExactCount(context.Background(), countCtx, q, opts)
		if err != nil {
			t.Fatal(err)
		}
		if counts != nil {
			t.Fatalf("counts = %#v, want unavailable", counts)
		}
		if diff := cmp.Diff(legacy.Files, result.Files); diff != "" {
			t.Fatalf("bounded fallback changed (-legacy +counted):\n%s", diff)
		}
	})

	t.Run("after display window", func(t *testing.T) {
		// The budget covers the first two documents and their line counts.
		// Expiring before the next document proves traversal stops once the
		// legacy window is already complete.
		countCtx := testutil.NewErrAfterContext(11)
		result, counts, err := searcher.SearchWithExactCount(context.Background(), countCtx, q, opts)
		if err != nil {
			t.Fatal(err)
		}
		if counts != nil {
			t.Fatalf("counts = %#v, want unavailable", counts)
		}
		if diff := cmp.Diff(legacy.Files, result.Files); diff != "" {
			t.Fatalf("bounded fallback changed (-legacy +counted):\n%s", diff)
		}
		if got := result.Stats.FilesConsidered; got != 2 {
			t.Fatalf("files considered = %d, want immediate stop at bounded window", got)
		}
		if got := result.Stats.FilesSkipped; got != 8 {
			t.Fatalf("files skipped = %d, want remaining 8 documents", got)
		}
	})

	t.Run("while matching after display window", func(t *testing.T) {
		countCtx := testutil.NewErrAfterContext(12)
		result, counts, err := searcher.SearchWithExactCount(context.Background(), countCtx, q, opts)
		if err != nil {
			t.Fatal(err)
		}
		if counts != nil {
			t.Fatalf("counts = %#v, want unavailable", counts)
		}
		if diff := cmp.Diff(legacy.Files, result.Files); diff != "" {
			t.Fatalf("bounded fallback changed (-legacy +counted):\n%s", diff)
		}
		if got := result.Stats.FilesConsidered; got != 3 {
			t.Fatalf("files considered = %d, want stop during the next match", got)
		}
		if got := result.Stats.FilesSkipped; got != 8 {
			t.Fatalf("files skipped = %d, want current and remaining 8 documents", got)
		}
	})
}

func TestSearchShardLimitPreservesFilesSkipped(t *testing.T) {
	searcher := exactCountSearcherForTest(t,
		Document{Name: "first.go", Content: []byte("needle\n")},
		Document{Name: "second.go", Content: []byte("needle\n")},
		Document{Name: "third.go", Content: []byte("needle\n")},
	)

	result, err := searcher.Search(
		context.Background(),
		&query.Substring{Pattern: "needle"},
		&zoekt.SearchOptions{ShardMaxMatchCount: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Stats.FilesSkipped; got != 2 {
		t.Fatalf("files skipped = %d, want remaining 2 documents", got)
	}
}

func TestIndexSearchModeRechecksBudgetBeforePublishingCounts(t *testing.T) {
	countCtx := testutil.NewErrAfterContext(1)
	mode, err := newIndexSearchMode(countCtx, &zoekt.SearchOptions{MaxDocDisplayCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	mode.counts = zoekt.ExactSearchCounts{MatchCount: 1, FileCount: 1}

	result, counts, err := mode.finish(context.Background(), &zoekt.SearchResult{}, &zoekt.SearchOptions{MaxDocDisplayCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("bounded result unexpectedly nil")
	}
	if counts != nil {
		t.Fatalf("counts = %#v, want unavailable after final budget check", counts)
	}
}

func TestIndexSearchModeRechecksRequestBeforePublishingResult(t *testing.T) {
	mode, err := newIndexSearchMode(context.Background(), &zoekt.SearchOptions{MaxDocDisplayCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	requestCtx := testutil.NewErrAfterContext(0)

	result, counts, err := mode.finish(requestCtx, &zoekt.SearchResult{}, &zoekt.SearchOptions{MaxDocDisplayCount: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if result != nil || counts != nil {
		t.Fatalf("result/counts = %#v/%#v, want no partial response", result, counts)
	}
}

func TestIndexSearchModeUsesUntruncatedMatchCountForLegacyLimit(t *testing.T) {
	opts := &zoekt.SearchOptions{
		ChunkMatches:         true,
		ShardMaxMatchCount:   2,
		MaxDocDisplayCount:   1,
		MaxMatchDisplayCount: 1,
	}
	mode, err := newIndexSearchMode(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	result := &zoekt.SearchResult{}
	file := zoekt.FileMatch{
		FileName: "many.go",
		ChunkMatches: []zoekt.ChunkMatch{{
			Ranges: make([]zoekt.Range, 3),
		}},
	}

	mode.addLegacyResult(result, file, 3, opts)

	if result.Stats.MatchCount != 3 {
		t.Fatalf("legacy match count = %d, want untruncated count 3", result.Stats.MatchCount)
	}
	if !mode.legacyDone {
		t.Fatal("legacy traversal did not stop at the untruncated shard match limit")
	}
	retained := mode.collector.Files(opts)
	if len(retained) != 1 || len(retained[0].ChunkMatches) != 1 || len(retained[0].ChunkMatches[0].Ranges) != 1 {
		t.Fatalf("retained result = %#v, want one display-bounded chunk range", retained)
	}
}

func TestSearchWithExactCountRequestCancellationAborts(t *testing.T) {
	searcher := exactCountSearcherForTest(t,
		Document{Name: "a.go", Content: []byte("needle\n")},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, counts, err := searcher.SearchWithExactCount(ctx, context.Background(), &query.Substring{Pattern: "needle"}, &zoekt.SearchOptions{MaxDocDisplayCount: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if result != nil || counts != nil {
		t.Fatalf("result/counts = %#v/%#v, want nil on request cancellation", result, counts)
	}

	t.Run("during traversal", func(t *testing.T) {
		docs := make([]Document, 100)
		for i := range docs {
			docs[i] = Document{Name: fmt.Sprintf("file-%03d.go", i), Content: []byte("needle\n")}
		}
		searcher := exactCountSearcherForTest(t, docs...)
		requestCtx := testutil.NewErrAfterContext(3)
		result, counts, err := searcher.SearchWithExactCount(
			requestCtx,
			context.Background(),
			&query.Substring{Pattern: "needle"},
			&zoekt.SearchOptions{MaxDocDisplayCount: 1},
		)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		if result != nil || counts != nil {
			t.Fatalf("result/counts = %#v/%#v, want no partial response", result, counts)
		}
	})

	t.Run("empty index", func(t *testing.T) {
		emptySearcher := exactCountSearcherForTest(t)
		requestCtx, cancel := context.WithCancel(context.Background())
		cancel()
		result, counts, err := emptySearcher.SearchWithExactCount(
			requestCtx,
			context.Background(),
			&query.Substring{Pattern: "needle"},
			&zoekt.SearchOptions{MaxDocDisplayCount: 1},
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if result != nil || counts != nil {
			t.Fatalf("result/counts = %#v/%#v, want nil on request cancellation", result, counts)
		}
	})
}

func TestSearchWithExactCountRequiresBoundedResults(t *testing.T) {
	searcher := exactCountSearcherForTest(t,
		Document{Name: "a.go", Content: []byte("needle\n")},
	)
	_, _, err := searcher.SearchWithExactCount(
		context.Background(),
		context.Background(),
		&query.Substring{Pattern: "needle"},
		&zoekt.SearchOptions{ShardMaxMatchCount: -1},
	)
	if !errors.Is(err, zoekt.ErrExactCountRequiresBoundedResults) {
		t.Fatalf("error = %v, want ErrExactCountRequiresBoundedResults", err)
	}
}

func TestSearchWithExactCountResultMemoryIsDisplayBounded(t *testing.T) {
	search := func(size int) (*zoekt.SearchResult, *zoekt.ExactSearchCounts) {
		t.Helper()
		docs := make([]Document, size)
		for i := range docs {
			docs[i] = Document{Name: fmt.Sprintf("file-%06d.go", i), Content: []byte("needle\n")}
		}
		searcher := exactCountSearcherForTest(t, docs...)
		result, counts, err := searcher.SearchWithExactCount(
			context.Background(),
			context.Background(),
			&query.Substring{Pattern: "needle"},
			&zoekt.SearchOptions{
				ShardMaxMatchCount:   15,
				TotalMaxMatchCount:   15,
				MaxDocDisplayCount:   15,
				MaxMatchDisplayCount: 15,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return result, counts
	}

	smallResult, smallCounts := search(100)
	largeResult, largeCounts := search(10_000)
	if smallCounts.MatchCount != 100 || largeCounts.MatchCount != 10_000 {
		t.Fatalf("exact match counts = %d/%d, want 100/10000", smallCounts.MatchCount, largeCounts.MatchCount)
	}
	if len(smallResult.Files) != 15 || len(largeResult.Files) != 15 {
		t.Fatalf("retained files = %d/%d, want 15/15", len(smallResult.Files), len(largeResult.Files))
	}
	if growth := int64(largeResult.SizeBytes()) - int64(smallResult.SizeBytes()); growth > 64<<10 {
		t.Fatalf("result memory grew by %d bytes with corpus matches; want <= 64 KiB", growth)
	}
}

func TestSearchWithExactCountPreservesNovelExtensionBoost(t *testing.T) {
	searcher := exactCountSearcherForTest(t,
		Document{Name: "first.go", Content: []byte("needle\n")},
		Document{Name: "second.go", Content: []byte("needle\n")},
		Document{Name: "third.go", Content: []byte("needle\n")},
		Document{Name: "novel.cpp", Content: []byte("needle\n")},
		Document{Name: "novel.rs", Content: []byte("needle\n")},
		Document{Name: "novel.py", Content: []byte("needle\n")},
		Document{Name: "novel.java", Content: []byte("needle\n")},
	)
	q := &query.Substring{Pattern: "needle"}
	opts := &zoekt.SearchOptions{
		ShardMaxMatchCount:   7,
		TotalMaxMatchCount:   7,
		MaxDocDisplayCount:   3,
		MaxMatchDisplayCount: 3,
	}
	legacy, err := searcher.Search(context.Background(), q, opts)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Files = SortAndTruncateFiles(legacy.Files, opts)
	counted, counts, err := searcher.SearchWithExactCount(context.Background(), context.Background(), q, opts)
	if err != nil {
		t.Fatal(err)
	}
	if counts == nil || counts.FileCount != 7 {
		t.Fatalf("counts = %#v, want seven files", counts)
	}
	if diff := cmp.Diff(legacy.Files, counted.Files); diff != "" {
		t.Fatalf("novel-extension bounded result changed (-legacy +counted):\n%s", diff)
	}
}

func exactCountSearcherForTest(t testing.TB, docs ...Document) zoekt.CountedSearcher {
	t.Helper()
	builder := testShardBuilder(t, nil, docs...)
	searcher := searcherForTest(t, builder)
	counted, ok := searcher.(zoekt.CountedSearcher)
	if !ok {
		t.Fatalf("%T does not implement zoekt.CountedSearcher", searcher)
	}
	return counted
}

func resultLineCount(result *zoekt.SearchResult) int {
	count := 0
	for _, file := range result.Files {
		count += len(file.LineMatches)
	}
	return count
}

func BenchmarkSearchWithExactCountBroadQuery(b *testing.B) {
	for _, size := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("files-%d", size), func(b *testing.B) {
			docs := make([]Document, size)
			for i := range docs {
				docs[i] = Document{
					Name:    fmt.Sprintf("Source/Module/File%06d.cpp", i),
					Content: []byte("needle broad match\n"),
				}
			}
			searcher := exactCountSearcherForTest(b, docs...)
			opts := &zoekt.SearchOptions{
				ShardMaxMatchCount:   15,
				TotalMaxMatchCount:   15,
				MaxDocDisplayCount:   15,
				MaxMatchDisplayCount: 15,
			}
			q := &query.Substring{Pattern: "needle"}
			var resultBytes uint64

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				result, counts, err := searcher.SearchWithExactCount(context.Background(), context.Background(), q, opts)
				if err != nil {
					b.Fatal(err)
				}
				if counts == nil || counts.MatchCount != size || counts.FileCount != size {
					b.Fatalf("counts = %#v, want %d matches/files", counts, size)
				}
				resultBytes = result.SizeBytes()
			}
			b.StopTimer()
			b.ReportMetric(float64(resultBytes), "result-B/op")
		})
	}
}
