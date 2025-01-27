// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package replication

import (
	"context"
	"sync"
	"sync/atomic"

	"go.temporal.io/server/api/historyservice/v1"
	"go.temporal.io/server/client"
	"go.temporal.io/server/client/history"
	"go.temporal.io/server/common"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/persistence/serialization"
	"go.temporal.io/server/common/xdc"
	"go.temporal.io/server/service/history/configs"
	"go.temporal.io/server/service/history/shard"
	"go.temporal.io/server/service/history/workflow"
)

type (
	// taskProcessorFactoryImpl is to manage replication task processors
	taskProcessorFactoryImpl struct {
		config                        *configs.Config
		deleteMgr                     workflow.DeleteManager
		engine                        shard.Engine
		eventSerializer               serialization.Serializer
		shard                         shard.Context
		status                        int32
		replicationTaskFetcherFactory TaskFetcherFactory
		workflowCache                 workflow.Cache
		resender                      xdc.NDCHistoryResender
		taskExecutorProvider          TaskExecutorProvider

		taskProcessorLock sync.RWMutex
		taskProcessors    map[string]TaskProcessor
	}
)

var _ common.Daemon = (*taskProcessorFactoryImpl)(nil)

func NewTaskProcessorManager(
	config *configs.Config,
	shard shard.Context,
	engine shard.Engine,
	workflowCache workflow.Cache,
	workflowDeleteManager workflow.DeleteManager,
	clientBean client.Bean,
	eventSerializer serialization.Serializer,
	replicationTaskFetcherFactory TaskFetcherFactory,
	taskExecutorProvider TaskExecutorProvider,
) *taskProcessorFactoryImpl {

	// Intentionally use the raw client to create its own retry policy
	historyClient := shard.GetHistoryClient()
	historyRetryableClient := history.NewRetryableClient(
		historyClient,
		common.CreateReplicationServiceBusyRetryPolicy(),
		common.IsResourceExhausted,
	)

	return &taskProcessorFactoryImpl{
		config:                        config,
		deleteMgr:                     workflowDeleteManager,
		engine:                        engine,
		eventSerializer:               eventSerializer,
		shard:                         shard,
		status:                        common.DaemonStatusInitialized,
		replicationTaskFetcherFactory: replicationTaskFetcherFactory,
		workflowCache:                 workflowCache,
		resender: xdc.NewNDCHistoryResender(
			shard.GetNamespaceRegistry(),
			clientBean,
			func(ctx context.Context, request *historyservice.ReplicateEventsV2Request) error {
				_, err := historyRetryableClient.ReplicateEventsV2(ctx, request)
				return err
			},
			shard.GetPayloadSerializer(),
			shard.GetConfig().StandbyTaskReReplicationContextTimeout,
			shard.GetLogger(),
		),
		taskProcessors:       make(map[string]TaskProcessor),
		taskExecutorProvider: taskExecutorProvider,
	}
}

func (r *taskProcessorFactoryImpl) Start() {
	if !atomic.CompareAndSwapInt32(
		&r.status,
		common.DaemonStatusInitialized,
		common.DaemonStatusStarted,
	) {
		return
	}

	// Listen to cluster metadata and dynamically update replication processor for remote clusters.
	r.listenToClusterMetadataChange()
}

func (r *taskProcessorFactoryImpl) Stop() {
	if !atomic.CompareAndSwapInt32(
		&r.status,
		common.DaemonStatusStarted,
		common.DaemonStatusStopped,
	) {
		return
	}

	r.shard.GetClusterMetadata().UnRegisterMetadataChangeCallback(r)
	r.taskProcessorLock.Lock()
	for _, replicationTaskProcessor := range r.taskProcessors {
		replicationTaskProcessor.Stop()
	}
	r.taskProcessorLock.Unlock()
}

func (r *taskProcessorFactoryImpl) listenToClusterMetadataChange() {
	clusterMetadata := r.shard.GetClusterMetadata()
	clusterMetadata.RegisterMetadataChangeCallback(
		r,
		r.handleClusterMetadataUpdate,
	)
}

func (r *taskProcessorFactoryImpl) handleClusterMetadataUpdate(
	oldClusterMetadata map[string]*cluster.ClusterInformation,
	newClusterMetadata map[string]*cluster.ClusterInformation,
) {
	r.taskProcessorLock.Lock()
	defer r.taskProcessorLock.Unlock()
	currentClusterName := r.shard.GetClusterMetadata().GetCurrentClusterName()
	for clusterName := range oldClusterMetadata {
		if clusterName == currentClusterName {
			continue
		}
		// The metadata triggers a update when the following fields update: 1. Enabled 2. Initial Failover Version 3. Cluster address
		// The callback covers three cases:
		// Case 1: Remove a cluster Case 2: Add a new cluster Case 3: Refresh cluster metadata.

		if processor, ok := r.taskProcessors[clusterName]; ok {
			// Case 1 and Case 3
			processor.Stop()
			delete(r.taskProcessors, clusterName)
		}

		if clusterInfo := newClusterMetadata[clusterName]; clusterInfo != nil && clusterInfo.Enabled {
			// Case 2 and Case 3
			fetcher := r.replicationTaskFetcherFactory.GetOrCreateFetcher(clusterName)
			replicationTaskProcessor := NewTaskProcessor(
				r.shard,
				r.engine,
				r.config,
				r.shard.GetMetricsClient(),
				fetcher,
				r.taskExecutorProvider(TaskExecutorParams{
					RemoteCluster:   clusterName,
					Shard:           r.shard,
					HistoryResender: r.resender,
					HistoryEngine:   r.engine,
					DeleteManager:   r.deleteMgr,
					WorkflowCache:   r.workflowCache,
				}),
				r.eventSerializer,
			)
			replicationTaskProcessor.Start()
			r.taskProcessors[clusterName] = replicationTaskProcessor
		}
	}
}
