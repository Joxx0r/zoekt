package index

import (
	"context"
	"path"
	"sort"

	"github.com/sourcegraph/zoekt"
)

const exactCountContextCheckInterval = 1024

type retainedFileMatch struct {
	sequence uint64
	match    zoekt.FileMatch
}

// indexSearchMode keeps the legacy result-window and optional exact-count
// policies out of the document traversal. Legacy Search uses the zero-value
// counting fields; SearchWithExactCount installs a separate count context and
// a bounded result collector.
type indexSearchMode struct {
	countCtx      context.Context
	countComplete bool
	counts        zoekt.ExactSearchCounts
	collector     *boundedFileCollector
	legacyDone    bool
}

type indexTraversalAction uint8

const (
	indexTraversalContinue indexTraversalAction = iota
	indexTraversalCanceled
	indexTraversalStop
	indexTraversalCollect
)

func newIndexSearchMode(countCtx context.Context, opts *zoekt.SearchOptions) (*indexSearchMode, error) {
	mode := &indexSearchMode{countCtx: countCtx}
	if countCtx == nil {
		return mode, nil
	}
	if opts.MaxDocDisplayCount <= 0 && opts.MaxMatchDisplayCount <= 0 && opts.ShardMaxMatchCount <= 0 {
		return nil, zoekt.ErrExactCountRequiresBoundedResults
	}
	mode.countComplete = countCtx.Err() == nil
	mode.collector = newBoundedFileCollector(opts)
	return mode, nil
}

func (m *indexSearchMode) exactRequested() bool {
	return m.countCtx != nil
}

func (m *indexSearchMode) refreshCountBudget() {
	if m.countComplete && m.countCtx.Err() != nil {
		m.countComplete = false
	}
}

func (m *indexSearchMode) requestState(ctx context.Context) (canceled bool, err error) {
	err = ctx.Err()
	if err != nil && m.exactRequested() {
		return false, err
	}
	return err != nil, nil
}

func (m *indexSearchMode) shouldStop() bool {
	return m.legacyDone && !m.countComplete
}

func (m *indexSearchMode) shouldSkipRepoLimited(repoLimited bool) bool {
	return repoLimited && !m.countComplete
}

func (m *indexSearchMode) shouldCollectLegacy(repoLimited bool) bool {
	return !m.legacyDone && !repoLimited
}

func (m *indexSearchMode) beforeDocument(ctx context.Context) (indexTraversalAction, error) {
	canceled, err := m.requestState(ctx)
	if err != nil {
		return indexTraversalContinue, err
	}
	if canceled {
		return indexTraversalCanceled, nil
	}
	m.refreshCountBudget()
	if m.shouldStop() {
		return indexTraversalStop, nil
	}
	return indexTraversalContinue, nil
}

func (m *indexSearchMode) afterDocumentMatch(ctx context.Context, repoLimited bool, cp *contentProvider, matches []*candidateMatch) (indexTraversalAction, error) {
	if _, err := m.requestState(ctx); err != nil {
		return indexTraversalContinue, err
	}
	m.refreshCountBudget()
	if m.shouldStop() {
		return indexTraversalStop, nil
	}
	if err := m.countDocument(ctx, cp, matches); err != nil {
		return indexTraversalContinue, err
	}
	if !m.shouldCollectLegacy(repoLimited) {
		if m.shouldStop() {
			return indexTraversalStop, nil
		}
		return indexTraversalContinue, nil
	}
	return indexTraversalCollect, nil
}

func (m *indexSearchMode) countDocument(ctx context.Context, cp *contentProvider, matches []*candidateMatch) error {
	if !m.countComplete {
		return nil
	}
	lineCount, complete, err := countSourceLines(ctx, m.countCtx, cp, matches)
	if err != nil {
		return err
	}
	if !complete {
		m.countComplete = false
		return nil
	}
	m.counts.MatchCount += lineCount
	m.counts.FileCount++
	return nil
}

func (m *indexSearchMode) addLegacyResult(result *zoekt.SearchResult, file zoekt.FileMatch, matchCount int, opts *zoekt.SearchOptions) {
	if m.collector != nil {
		m.collector.Add(file)
	} else {
		result.Files = append(result.Files, file)
	}

	result.Stats.MatchCount += matchCount
	result.Stats.FileCount++
	if opts.ShardMaxMatchCount > 0 && result.Stats.MatchCount >= opts.ShardMaxMatchCount {
		m.legacyDone = true
	}
}

func (m *indexSearchMode) finish(result *zoekt.SearchResult, opts *zoekt.SearchOptions) (*zoekt.SearchResult, *zoekt.ExactSearchCounts, error) {
	if m.collector != nil {
		result.Files = m.collector.Files(opts)
	}
	m.refreshCountBudget()
	if m.countComplete {
		return result, &m.counts, nil
	}
	return result, nil, nil
}

// boundedFileCollector retains enough score leaders to reproduce SortFiles'
// bounded output, including its third-result novel-extension promotion. The
// number of retained files is bounded by the requested display window plus
// three extension leaders, independent of the corpus match count.
type boundedFileCollector struct {
	limit            int
	matchLimit       int
	chunkMatches     bool
	nextSequence     uint64
	scoreLeaders     []retainedFileMatch
	extensionLeaders []retainedFileMatch
}

func newBoundedFileCollector(opts *zoekt.SearchOptions) *boundedFileCollector {
	limit := max(opts.MaxDocDisplayCount, opts.MaxMatchDisplayCount)
	if limit <= 0 {
		limit = opts.ShardMaxMatchCount
	}
	return &boundedFileCollector{
		limit:        limit,
		matchLimit:   opts.MaxMatchDisplayCount,
		chunkMatches: opts.ChunkMatches,
	}
}

func (c *boundedFileCollector) Add(file zoekt.FileMatch) {
	c.limitFileMatches(&file)

	candidate := retainedFileMatch{sequence: c.nextSequence, match: file}
	c.nextSequence++
	c.addScoreLeader(candidate)
	c.addExtensionLeader(candidate)
}

func (c *boundedFileCollector) limitFileMatches(file *zoekt.FileMatch) {
	if c.matchLimit > 0 {
		if c.chunkMatches {
			limitChunkMatches(file, c.matchLimit)
		} else {
			limitLineMatches(file, c.matchLimit)
		}
	}
}

func (c *boundedFileCollector) addScoreLeader(candidate retainedFileMatch) {
	c.scoreLeaders = append(c.scoreLeaders, candidate)
	sortRetainedFiles(c.scoreLeaders)
	if len(c.scoreLeaders) > c.limit {
		c.scoreLeaders = c.scoreLeaders[:c.limit]
	}
}

func (c *boundedFileCollector) addExtensionLeader(candidate retainedFileMatch) {
	ext := path.Ext(candidate.match.FileName)
	for i := range c.extensionLeaders {
		if path.Ext(c.extensionLeaders[i].match.FileName) != ext {
			continue
		}
		if retainedFileLess(candidate, c.extensionLeaders[i]) {
			c.extensionLeaders[i] = candidate
			sortRetainedFiles(c.extensionLeaders)
		}
		return
	}

	c.extensionLeaders = append(c.extensionLeaders, candidate)
	sortRetainedFiles(c.extensionLeaders)
	if len(c.extensionLeaders) > 3 {
		c.extensionLeaders = c.extensionLeaders[:3]
	}
}

func (c *boundedFileCollector) Files(opts *zoekt.SearchOptions) []zoekt.FileMatch {
	bySequence := make(map[uint64]zoekt.FileMatch, len(c.scoreLeaders)+len(c.extensionLeaders))
	for _, candidate := range c.scoreLeaders {
		bySequence[candidate.sequence] = candidate.match
	}
	for _, candidate := range c.extensionLeaders {
		bySequence[candidate.sequence] = candidate.match
	}

	retained := make([]retainedFileMatch, 0, len(bySequence))
	for sequence, file := range bySequence {
		retained = append(retained, retainedFileMatch{sequence: sequence, match: file})
	}
	sortRetainedFiles(retained)

	files := make([]zoekt.FileMatch, 0, len(retained))
	for _, candidate := range retained {
		files = append(files, candidate.match)
	}
	return SortAndTruncateFiles(files, opts)
}

func sortRetainedFiles(files []retainedFileMatch) {
	sort.Slice(files, func(i, j int) bool {
		return retainedFileLess(files[i], files[j])
	})
}

func retainedFileLess(a, b retainedFileMatch) bool {
	if a.match.Score != b.match.Score {
		return a.match.Score > b.match.Score
	}
	return a.sequence < b.sequence
}

// countSourceLines mirrors fillMatches' identity semantics without building
// LineMatch content or context payloads. Content matches suppress filename
// matches, and multiple alternatives on the same source line count once.
func countSourceLines(ctx, countCtx context.Context, cp *contentProvider, matches []*candidateMatch) (int, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if countCtx.Err() != nil {
		return 0, false, nil
	}
	hasContent := false
	for _, match := range matches {
		if !match.fileName {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return 1, true, nil
	}

	data := cp.data(false)
	lastLine := -1
	lineCount := 0
	checks := 0
	addLine := func(offset uint32) {
		line := cp.newlines().atOffset(offset)
		if line != lastLine {
			lastLine = line
			lineCount++
		}
	}

	for _, match := range matches {
		if match.fileName {
			continue
		}
		start := match.byteOffset
		end := match.byteOffset + match.byteMatchSz
		// Every content candidate owns its starting source line, including
		// zero-width matches and candidates that consist only of a newline.
		addLine(start)
		for offset := start; offset < end; offset++ {
			if checks%exactCountContextCheckInterval == 0 {
				if err := ctx.Err(); err != nil {
					return 0, false, err
				}
				if countCtx.Err() != nil {
					return 0, false, nil
				}
			}
			checks++
			if data[offset] != '\n' {
				continue
			}
			if offset+1 < end {
				addLine(offset + 1)
			}
		}
	}
	return lineCount, true, nil
}
