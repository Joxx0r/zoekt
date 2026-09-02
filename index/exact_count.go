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
	if c.matchLimit > 0 {
		if c.chunkMatches {
			limitChunkMatches(&file, c.matchLimit)
		} else {
			limitLineMatches(&file, c.matchLimit)
		}
	}

	candidate := retainedFileMatch{sequence: c.nextSequence, match: file}
	c.nextSequence++

	c.scoreLeaders = append(c.scoreLeaders, candidate)
	sortRetainedFiles(c.scoreLeaders)
	if len(c.scoreLeaders) > c.limit {
		c.scoreLeaders = c.scoreLeaders[:c.limit]
	}

	ext := path.Ext(file.FileName)
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
		segmentStart := start
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
			if segmentStart < offset {
				addLine(segmentStart)
			}
			segmentStart = offset + 1
		}
		if segmentStart < end {
			addLine(segmentStart)
		}
	}
	return lineCount, true, nil
}
