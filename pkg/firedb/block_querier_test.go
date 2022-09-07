package firedb

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/go-kit/log"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonv1 "github.com/grafana/fire/pkg/gen/common/v1"
	ingestv1 "github.com/grafana/fire/pkg/gen/ingester/v1"
	"github.com/grafana/fire/pkg/iter"
	"github.com/grafana/fire/pkg/objstore/providers/filesystem"
	pprofth "github.com/grafana/fire/pkg/pprof/testhelper"
)

func Test_BlockQuerier(t *testing.T) {
	tsdbPath := t.TempDir()
	head, err := NewHead(tsdbPath)
	require.NoError(t, err)

	ctx := context.Background()

	var p *pprofth.ProfileBuilder

	for pos := range [2001]struct{}{} {
		p := pprofth.NewProfileBuilder(1000 + int64(pos)).MemoryProfile()
		p.ForStacktrace("my", "stack").AddSamples(5+int64(pos), 2+int64(pos), 5+int64(pos), 2+int64(pos))
		require.NoError(t, head.Ingest(ctx, p.Profile, p.UUID, append(p.Labels, &commonv1.LabelPair{Name: "test", Value: "label"})...))
	}

	p = pprofth.NewProfileBuilder(1001).MemoryProfile()
	p.ForStacktrace("my", "other", "stack").AddSamples(3, 2, 1, 0)
	require.NoError(t, head.Ingest(ctx, p.Profile, p.UUID, p.Labels...))
	p = pprofth.NewProfileBuilder(1002).MemoryProfile()
	p.ForStacktrace("my", "other", "stack").AddSamples(4, 3, 2, 1)
	require.NoError(t, head.Ingest(ctx, p.Profile, p.UUID, p.Labels...))
	p = pprofth.NewProfileBuilder(1003).CPUProfile()
	p.ForStacktrace("my", "stack").AddSamples(1234)
	require.NoError(t, head.Ingest(ctx, p.Profile, p.UUID, p.Labels...))

	// no flush the head to disk
	require.NoError(t, head.Flush(ctx))

	blockPath := filepath.Join(tsdbPath, pathLocal)

	b, err := filesystem.NewBucket(blockPath)
	require.NoError(t, err)

	// open resulting block
	q := NewBlockQuerier(log.NewNopLogger(), b)
	require.NoError(t, q.Sync(context.Background()))

	result, err := q.SelectProfiles(ctx, connect.NewRequest(&ingestv1.SelectProfilesRequest{
		LabelSelector: `{test="label"}`,
		Type: &commonv1.ProfileType{
			Name:       "memory",
			SampleType: "alloc_space",
			SampleUnit: "bytes",
			PeriodType: "space",
			PeriodUnit: "bytes",
		},
		Start: 0,
		End:   200000,
	}))
	require.NoError(t, err)
	require.Equal(t, 2000, len(result.Msg.Profiles))
	profile := result.Msg.Profiles[0]

	// ensure there is at least a stacktrace
	require.Greater(t, len(profile.Stacktraces), 0)
	require.Equal(t, 2, len(profile.Stacktraces[0].FunctionIds))
	assert.Equal(t, "my", result.Msg.FunctionNames[profile.Stacktraces[0].FunctionIds[0]])
	assert.Equal(t, "stack", result.Msg.FunctionNames[profile.Stacktraces[0].FunctionIds[1]])

	result, err = q.SelectProfiles(ctx, connect.NewRequest(&ingestv1.SelectProfilesRequest{
		LabelSelector: `{test!="label"}`,
		Type: &commonv1.ProfileType{
			Name:       "memory",
			SampleType: "alloc_space",
			SampleUnit: "bytes",
			PeriodType: "space",
			PeriodUnit: "bytes",
		},
		Start: 0,
		End:   200000,
	}))
	require.NoError(t, err)

	// ensure there is a profile
	require.Equal(t, 1, len(result.Msg.Profiles))
	profile = result.Msg.Profiles[0]

	// ensure there is at least a stacktrace
	require.Greater(t, len(profile.Stacktraces), 0)
	require.Equal(t, 3, len(profile.Stacktraces[0].FunctionIds))
	assert.Equal(t, "my", result.Msg.FunctionNames[profile.Stacktraces[0].FunctionIds[0]])
	assert.Equal(t, "other", result.Msg.FunctionNames[profile.Stacktraces[0].FunctionIds[1]])
	assert.Equal(t, "stack", result.Msg.FunctionNames[profile.Stacktraces[0].FunctionIds[2]])
}

func TestBlockQuerierMerger(t *testing.T) {
	testPath := t.TempDir()
	db, err := New(&Config{
		DataPath:      testPath,
		BlockDuration: time.Duration(100000) * time.Minute, // we will manually flush
	}, log.NewNopLogger(), nil)
	require.NoError(t, err)
	ctx := context.Background()

	p := pprofth.NewProfileBuilder(int64(15 * time.Second)).CPUProfile()
	p.ForStacktrace("my", "other").AddSamples(1)
	p.ForStacktrace("my", "other").AddSamples(3)
	p.ForStacktrace("my", "other", "stack").AddSamples(3)
	// require.NoError(t, db.Head().Ingest(ctx, p.Profile, p.UUID, p.Labels...))
	p = pprofth.NewProfileBuilder(int64(30 * time.Second)).CPUProfile()
	p.ForStacktrace("my", "other").AddSamples(2)
	p.ForStacktrace("my", "other").AddSamples(6)
	p.ForStacktrace("my", "other", "stack").AddSamples(6)
	// require.NoError(t, db.Head().Ingest(ctx, p.Profile, p.UUID, p.Labels...))
	p = pprofth.NewProfileBuilder(int64(35 * time.Second)).MemoryProfile()
	p.ForStacktrace("my", "other").AddSamples(2, 3, 4, 5)
	p.ForStacktrace("my", "other", "stack").AddSamples(6, 7, 8, 9)
	require.NoError(t, db.Head().Ingest(ctx, p.Profile, p.UUID, p.Labels...))

	p = pprofth.NewProfileBuilder(int64(35 * time.Second)).MemoryProfile()
	p.ForStacktrace("my", "other").AddSamples(4, 6, 8, 10)
	p.ForStacktrace("my", "other", "stack").AddSamples(12, 14, 16, 18)
	require.NoError(t, db.Head().Ingest(ctx, p.Profile, p.UUID, p.Labels...))

	require.NoError(t, db.Flush(context.Background()))

	b, err := filesystem.NewBucket(filepath.Join(testPath, pathLocal))
	require.NoError(t, err)

	// open resulting block
	q := NewBlockQuerier(log.NewNopLogger(), b)
	require.NoError(t, q.Sync(context.Background()))

	merger, err := q.queriers[0].SelectMerge(ctx, SelectMergeRequest{
		LabelSelector: `{}`,
		Type: &commonv1.ProfileType{
			Name:       "process_cpu",
			SampleType: "cpu",
			SampleUnit: "nanoseconds",
			PeriodType: "cpu",
			PeriodUnit: "nanoseconds",
		},
		Start: model.TimeFromUnixNano(0),
		End:   model.TimeFromUnixNano(int64(1 * time.Minute)),
	})
	require.NoError(t, err)
	profiles := merger.SelectedProfiles()
	// for profiles.Next() {
	// 	p := profiles.At()
	// 	fmt.Println(p)
	// }
	// require.NoError(t, profiles.Err())
	stacktraceIter := merger.MergeByStacktraces(AllProfile{profiles})
	for stacktraceIter.Next() {
		stacktrace := stacktraceIter.At()
		fmt.Println(stacktrace)
	}
	require.NoError(t, stacktraceIter.Close())

	merger, err = q.queriers[0].SelectMerge(ctx, SelectMergeRequest{
		LabelSelector: `{}`,
		Type: &commonv1.ProfileType{
			Name:       "memory",
			SampleType: "inuse_space",
			SampleUnit: "bytes",
			PeriodType: "space",
			PeriodUnit: "bytes",
		},
		Start: model.TimeFromUnixNano(0),
		End:   model.TimeFromUnixNano(int64(1 * time.Minute)),
	})
	require.NoError(t, err)
	profiles = merger.SelectedProfiles()

	stacktraceIter = merger.MergeByStacktraces(AllProfile{profiles})
	for stacktraceIter.Next() {
		stacktrace := stacktraceIter.At()
		fmt.Println(stacktrace)
	}
	require.NoError(t, stacktraceIter.Close())
}

type AllProfile struct {
	iter.Iterator[*Profile]
}

func (iter AllProfile) At() int64 {
	return iter.Iterator.At().RowNum
}

func Test_mergeSelectProfilesResponse(t *testing.T) {
	exp := &ingestv1.SelectProfilesResponse{
		Profiles: []*ingestv1.Profile{
			{
				ID: "id1",
				Stacktraces: []*ingestv1.StacktraceSample{
					{FunctionIds: []int32{0}, Value: 1},
					{FunctionIds: []int32{1}, Value: 2},
				},
			},
			{
				ID: "id2",
				Stacktraces: []*ingestv1.StacktraceSample{
					{FunctionIds: []int32{1}, Value: 3},
					{FunctionIds: []int32{2}, Value: 4},
				},
			},
			{
				ID: "id3",
				Stacktraces: []*ingestv1.StacktraceSample{
					{FunctionIds: []int32{1}, Value: 5},
				},
			},
		},
		FunctionNames: []string{"method-a", "method-b", "method-c"},
	}
	sharedFunctionIDs := []int32{0}
	act := mergeSelectProfilesResponse(
		&ingestv1.SelectProfilesResponse{},
		&ingestv1.SelectProfilesResponse{
			Profiles: []*ingestv1.Profile{
				{
					ID: "id1",
					Stacktraces: []*ingestv1.StacktraceSample{
						{FunctionIds: []int32{0}, Value: 1},
						{FunctionIds: []int32{1}, Value: 2},
					},
				},
			},
			FunctionNames: []string{"method-a", "method-b"},
		},
		&ingestv1.SelectProfilesResponse{
			Profiles: []*ingestv1.Profile{
				{
					ID: "id2",
					Stacktraces: []*ingestv1.StacktraceSample{
						{FunctionIds: sharedFunctionIDs, Value: 3},
						{FunctionIds: []int32{1}, Value: 4},
					},
				},
				{
					ID: "id3",
					Stacktraces: []*ingestv1.StacktraceSample{
						{FunctionIds: sharedFunctionIDs, Value: 5},
					},
				},
			},
			FunctionNames: []string{"method-b", "method-c"},
		},
	)

	if diff := cmp.Diff(exp, act, cmpopts.IgnoreUnexported(ingestv1.SelectProfilesResponse{}, ingestv1.Profile{}, ingestv1.StacktraceSample{})); diff != "" {
		t.Errorf("Unexpected mergeSelectProfilesResponse result(-expect +actual):\n%s", diff)
	}
}
