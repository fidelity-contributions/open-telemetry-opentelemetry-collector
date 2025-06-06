// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package queuebatch // import "go.opentelemetry.io/collector/exporter/exporterhelper/internal/queuebatch"
import (
	"context"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/exporter/exporterhelper/internal/request"
	"go.opentelemetry.io/collector/exporter/exporterhelper/internal/sender"
)

type batcherSettings[T any] struct {
	sizerType   request.SizerType
	sizer       request.Sizer[T]
	partitioner Partitioner[T]
	next        sender.SendFunc[T]
	maxWorkers  int
}

type multiBatcher struct {
	cfg         BatchConfig
	wp          chan struct{}
	sizerType   request.SizerType
	sizer       request.Sizer[request.Request]
	partitioner Partitioner[request.Request]
	consumeFunc sender.SendFunc[request.Request]

	singleShard *shardBatcher
	shards      sync.Map
}

var _ Batcher[request.Request] = (*multiBatcher)(nil)

func newMultiBatcher(bCfg BatchConfig, bSet batcherSettings[request.Request]) *multiBatcher {
	var workerPool chan struct{}
	if bSet.maxWorkers != 0 {
		workerPool = make(chan struct{}, bSet.maxWorkers)
		for i := 0; i < bSet.maxWorkers; i++ {
			workerPool <- struct{}{}
		}
	}
	mb := &multiBatcher{
		cfg:         bCfg,
		wp:          workerPool,
		sizerType:   bSet.sizerType,
		sizer:       bSet.sizer,
		partitioner: bSet.partitioner,
		consumeFunc: bSet.next,
	}

	if bSet.partitioner == nil {
		mb.singleShard = newShard(mb.cfg, mb.sizerType, mb.sizer, mb.wp, mb.consumeFunc)
	}
	return mb
}

func (mb *multiBatcher) getShard(ctx context.Context, req request.Request) *shardBatcher {
	if mb.singleShard != nil {
		return mb.singleShard
	}

	key := mb.partitioner.GetKey(ctx, req)
	// Fast path, shard already created.
	s, found := mb.shards.Load(key)
	if found {
		return s.(*shardBatcher)
	}
	newS := newShard(mb.cfg, mb.sizerType, mb.sizer, mb.wp, mb.consumeFunc)
	newS.start(ctx, nil)
	s, loaded := mb.shards.LoadOrStore(key, newS)
	// If not loaded, there was a race condition in adding the new shard. Shutdown the newly created shard.
	if loaded {
		newS.shutdown(ctx)
	}
	return s.(*shardBatcher)
}

func (mb *multiBatcher) Start(ctx context.Context, host component.Host) error {
	if mb.singleShard != nil {
		mb.singleShard.start(ctx, host)
	}
	return nil
}

func (mb *multiBatcher) Consume(ctx context.Context, req request.Request, done Done) {
	shard := mb.getShard(ctx, req)
	shard.Consume(ctx, req, done)
}

func (mb *multiBatcher) Shutdown(ctx context.Context) error {
	if mb.singleShard != nil {
		mb.singleShard.shutdown(ctx)
		return nil
	}

	var wg sync.WaitGroup
	mb.shards.Range(func(_ any, shard any) bool {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shard.(*shardBatcher).shutdown(ctx)
		}()
		return true
	})
	wg.Wait()
	return nil
}
