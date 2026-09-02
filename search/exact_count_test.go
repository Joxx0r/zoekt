package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
)

func TestShardedSearchWithExactCountAggregatesRealShards(t *testing.T) {
	ss := newShardedSearcher(2)
	ss.replace(map[string]zoekt.Searcher{
		"one": searcherForTest(t, testShardBuilder(t,
			&zoekt.Repository{ID: 1, Name: "one", Rank: 100},
			index.Document{Name: "a.go", Content: []byte("needle needle\nneedle\n")},
		)),
		"two": searcherForTest(t, testShardBuilder(t,
			&zoekt.Repository{ID: 2, Name: "two"},
			index.Document{Name: "b.go", Content: []byte("needle\n")},
			index.Document{Name: "c.go", Content: []byte("needle\n")},
		)),
	})
	ss.markReady()

	q := &query.Or{Children: []query.Q{
		&query.Substring{Pattern: "needle"},
		&query.Substring{Pattern: "needle"},
	}}
	opts := &zoekt.SearchOptions{
		ShardMaxMatchCount:   1,
		TotalMaxMatchCount:   100,
		MaxDocDisplayCount:   1,
		MaxMatchDisplayCount: 1,
	}
	legacy, err := ss.Search(context.Background(), q, opts)
	if err != nil {
		t.Fatal(err)
	}
	result, counts, err := ss.SearchWithExactCount(context.Background(), context.Background(), q, opts)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(&zoekt.ExactSearchCounts{MatchCount: 4, FileCount: 3}, counts); diff != "" {
		t.Fatalf("exact counts mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(legacy.Files, result.Files); diff != "" {
		t.Fatalf("bounded multi-shard result changed (-legacy +counted):\n%s", diff)
	}

	scoped := &query.And{Children: []query.Q{
		&query.RepoSet{Set: map[string]bool{"one": true}},
		&query.Substring{Pattern: "needle"},
	}}
	_, scopedCounts, err := ss.SearchWithExactCount(context.Background(), context.Background(), scoped, opts)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(&zoekt.ExactSearchCounts{MatchCount: 2, FileCount: 1}, scopedCounts); diff != "" {
		t.Fatalf("scoped exact counts mismatch (-want +got):\n%s", diff)
	}
}

func TestShardedSearchWithExactCountBudgetExpiryPreservesResult(t *testing.T) {
	ss := newShardedSearcher(1)
	ss.replace(map[string]zoekt.Searcher{
		"one": searcherForTest(t, testShardBuilder(t,
			&zoekt.Repository{ID: 1, Name: "one"},
			index.Document{Name: "a.go", Content: []byte("needle\n")},
			index.Document{Name: "b.go", Content: []byte("needle\n")},
		)),
	})
	ss.markReady()
	opts := &zoekt.SearchOptions{
		ShardMaxMatchCount:   1,
		TotalMaxMatchCount:   1,
		MaxDocDisplayCount:   1,
		MaxMatchDisplayCount: 1,
	}
	q := &query.Substring{Pattern: "needle"}
	legacy, err := ss.Search(context.Background(), q, opts)
	if err != nil {
		t.Fatal(err)
	}
	countCtx, cancel := context.WithCancel(context.Background())
	cancel()
	result, counts, err := ss.SearchWithExactCount(context.Background(), countCtx, q, opts)
	if err != nil {
		t.Fatal(err)
	}
	if counts != nil {
		t.Fatalf("counts = %#v, want unavailable", counts)
	}
	if diff := cmp.Diff(legacy.Files, result.Files); diff != "" {
		t.Fatalf("bounded fallback changed (-legacy +counted):\n%s", diff)
	}
}

func TestShardedSearchWithExactCountRejectsUnboundedEmptySearch(t *testing.T) {
	ss := newShardedSearcher(1)
	ss.markReady()

	result, counts, err := ss.SearchWithExactCount(
		context.Background(),
		context.Background(),
		&query.Substring{Pattern: "needle"},
		&zoekt.SearchOptions{ShardMaxMatchCount: -1},
	)
	if !errors.Is(err, zoekt.ErrExactCountRequiresBoundedResults) {
		t.Fatalf("error = %v, want ErrExactCountRequiresBoundedResults", err)
	}
	if result != nil || counts != nil {
		t.Fatalf("result/counts = %#v/%#v, want nil for invalid options", result, counts)
	}
}

func TestShardedSearchWithExactCountReturnsZeroCountsForEmptySearch(t *testing.T) {
	ss := newShardedSearcher(1)
	ss.markReady()

	result, counts, err := ss.SearchWithExactCount(
		context.Background(),
		context.Background(),
		&query.Substring{Pattern: "needle"},
		&zoekt.SearchOptions{MaxDocDisplayCount: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Files) != 0 {
		t.Fatalf("result = %#v, want an empty result", result)
	}
	if diff := cmp.Diff(&zoekt.ExactSearchCounts{}, counts); diff != "" {
		t.Fatalf("exact counts mismatch (-want +got):\n%s", diff)
	}
}

func TestNewShardSearchCoordinatorHandlesNoShards(t *testing.T) {
	search := make(chan *shardSearchWork)
	coordinator := newShardSearchCoordinator(
		newExactShardSearchMode(context.Background()),
		&zoekt.SearchOptions{MaxDocDisplayCount: 1},
		zoekt.SenderFunc(func(*zoekt.SearchResult) {}),
		nil,
		search,
		0,
	)
	if coordinator.work != nil || coordinator.next != nil {
		t.Fatalf("empty coordinator remained schedulable: %#v", coordinator)
	}
	if _, open := <-search; open {
		t.Fatal("empty coordinator did not close its worker channel")
	}
}

func TestShardSearchModeRechecksBudgetBeforePublishingCounts(t *testing.T) {
	countCtx := newShardErrAfterContext(1)
	mode := newExactShardSearchMode(countCtx)
	mode.counts = zoekt.ExactSearchCounts{MatchCount: 1, FileCount: 1}

	if counts := mode.exactCounts(); counts != nil {
		t.Fatalf("counts = %#v, want unavailable after final budget check", counts)
	}
}

func TestShardedSearchWithExactCountRequestCancellation(t *testing.T) {
	ss := newShardedSearcher(1)
	ss.replace(map[string]zoekt.Searcher{
		"one": searcherForTest(t, testShardBuilder(t,
			&zoekt.Repository{ID: 1, Name: "one"},
			index.Document{Name: "a.go", Content: []byte("needle\n")},
		)),
	})
	ss.markReady()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := ss.SearchWithExactCount(ctx, context.Background(), &query.Substring{Pattern: "needle"}, &zoekt.SearchOptions{MaxDocDisplayCount: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestTypeRepoSearcherExposesExactCountContract(t *testing.T) {
	ss := newShardedSearcher(1)
	ss.replace(map[string]zoekt.Searcher{
		"one": searcherForTest(t, testShardBuilder(t,
			&zoekt.Repository{ID: 1, Name: "one"},
			index.Document{Name: "a.go", Content: []byte("needle\n")},
		)),
	})
	ss.markReady()
	wrapped := &typeRepoSearcher{Streamer: &directorySearcher{Streamer: ss}}
	counted, ok := any(wrapped).(zoekt.CountedSearcher)
	if !ok {
		t.Fatalf("%T does not implement zoekt.CountedSearcher", wrapped)
	}

	result, counts, err := counted.SearchWithExactCount(
		context.Background(),
		context.Background(),
		&query.Substring{Pattern: "needle"},
		&zoekt.SearchOptions{MaxDocDisplayCount: 1, MaxMatchDisplayCount: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || counts == nil || counts.MatchCount != 1 || counts.FileCount != 1 {
		t.Fatalf("result/counts = %#v/%#v, want one exact line in one file", result.Files, counts)
	}
}

func TestDirectorySearcherExposesExactCountContract(t *testing.T) {
	dir := t.TempDir()
	builder := testShardBuilder(t,
		&zoekt.Repository{ID: 1, Name: "one"},
		index.Document{Name: "a.go", Content: []byte("needle\nneedle\n")},
	)
	shard, err := os.Create(filepath.Join(dir, "one_v16.00000.zoekt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Write(shard); err != nil {
		shard.Close()
		t.Fatal(err)
	}
	if err := shard.Close(); err != nil {
		t.Fatal(err)
	}

	streamer, err := NewDirectorySearcher(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		streamer.Close()
		runtime.GC()
	}()
	counted, ok := streamer.(zoekt.CountedSearcher)
	if !ok {
		t.Fatalf("%T does not implement zoekt.CountedSearcher", streamer)
	}
	result, counts, err := counted.SearchWithExactCount(
		context.Background(),
		context.Background(),
		&query.Substring{Pattern: "needle"},
		&zoekt.SearchOptions{MaxDocDisplayCount: 1, MaxMatchDisplayCount: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || counts == nil || counts.MatchCount != 2 || counts.FileCount != 1 {
		t.Fatalf("result/counts = %#v/%#v, want two exact lines in one file", result.Files, counts)
	}
}

type shardErrAfterContext struct {
	context.Context
	allowed int
	calls   int
	done    chan struct{}
	err     error
}

func (c *shardErrAfterContext) Err() error {
	if c.err != nil {
		return c.err
	}
	c.calls++
	if c.calls > c.allowed {
		c.err = context.DeadlineExceeded
		close(c.done)
	}
	return c.err
}

func (c *shardErrAfterContext) Done() <-chan struct{} {
	return c.done
}

func newShardErrAfterContext(allowed int) *shardErrAfterContext {
	return &shardErrAfterContext{
		Context: context.Background(),
		allowed: allowed,
		done:    make(chan struct{}),
	}
}
