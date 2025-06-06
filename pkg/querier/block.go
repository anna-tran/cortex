package querier

import (
	"context"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/thanos-io/thanos/pkg/store/storepb"

	"github.com/cortexproject/cortex/pkg/storage/tsdb/bucketindex"
)

type contextKey int

var (
	blockCtxKey contextKey = 0
)

func InjectBlocksIntoContext(ctx context.Context, blocks ...*bucketindex.Block) context.Context {
	return context.WithValue(ctx, blockCtxKey, blocks)
}

func ExtractBlocksFromContext(ctx context.Context) ([]*bucketindex.Block, bool) {
	if blocks := ctx.Value(blockCtxKey); blocks != nil {
		return blocks.([]*bucketindex.Block), true
	}

	return nil, false
}

func convertMatchersToLabelMatcher(matchers []*labels.Matcher) []storepb.LabelMatcher {
	var converted []storepb.LabelMatcher
	for _, m := range matchers {
		var t storepb.LabelMatcher_Type
		switch m.Type {
		case labels.MatchEqual:
			t = storepb.LabelMatcher_EQ
		case labels.MatchNotEqual:
			t = storepb.LabelMatcher_NEQ
		case labels.MatchRegexp:
			t = storepb.LabelMatcher_RE
		case labels.MatchNotRegexp:
			t = storepb.LabelMatcher_NRE
		}

		converted = append(converted, storepb.LabelMatcher{
			Type:  t,
			Name:  m.Name,
			Value: m.Value,
		})
	}
	return converted
}

// storeSeriesSet implements a storepb SeriesSet against a list of storepb.Series.
type storeSeriesSet struct {
	series []*storepb.Series
	i      int
}

func newStoreSeriesSet(s []*storepb.Series) *storeSeriesSet {
	return &storeSeriesSet{series: s, i: -1}
}

func (s *storeSeriesSet) Next() bool {
	if s.i >= len(s.series)-1 {
		return false
	}
	s.i++
	return true
}

func (*storeSeriesSet) Err() error {
	return nil
}

func (s *storeSeriesSet) At() (labels.Labels, []storepb.AggrChunk) {
	return s.series[s.i].PromLabels(), s.series[s.i].Chunks
}
