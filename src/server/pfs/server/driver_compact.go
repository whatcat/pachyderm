package server

import (
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/fileset"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/fileset/index"
	"github.com/pachyderm/pachyderm/src/server/pkg/work"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

func (d *driver) compact(master *work.Master, ids []fileset.ID) (*fileset.ID, error) {
	// serialize access to RunSubtasks, the compactor may call workerFunc concurrently
	mu := sync.Mutex{}
	workerFunc := func(ctx context.Context, task fileset.CompactionTask) (*fileset.ID, error) {
		any, err := serializeCompactionTask(&pfs.CompactionTask{
			Inputs: task.Inputs,
			Range: &pfs.PathRange{
				Lower: task.PathRange.Lower,
				Upper: task.PathRange.Upper,
			},
		})
		if err != nil {
			return nil, err
		}
		workTasks := []*work.Task{&work.Task{Data: any}}
		mu.Lock()
		defer mu.Unlock()
		var result *fileset.ID
		if err := master.RunSubtasks(workTasks, func(_ context.Context, taskInfo *work.TaskInfo) error {
			if taskInfo.Result == nil {
				return errors.Errorf("no result set for compaction work.TaskInfo")
			}
			res, err := deserializeCompactionResult(taskInfo.Result)
			if err != nil {
				return err
			}
			id := fileset.ID(res.Id)
			result = &id
			return nil
		}); err != nil {
			return nil, err
		}
		return result, nil
	}
	dc := fileset.NewDistributedCompactor(d.storage, d.env.StorageCompactionMaxFanIn, workerFunc)
	return dc.Compact(master.Ctx(), ids, defaultTTL)
}

func (d *driver) compactionWorker() {
	ctx := context.Background()
	w := work.NewWorker(d.etcdClient, d.prefix, storageTaskNamespace)
	err := backoff.RetryNotify(func() error {
		return w.Run(ctx, func(ctx context.Context, subtask *work.Task) (*types.Any, error) {
			task, err := deserializeCompactionTask(subtask.Data)
			if err != nil {
				return nil, err
			}
			ids := []fileset.ID{}
			for _, input := range task.Inputs {
				id := fileset.ID(input)
				ids = append(ids, id)
			}
			pathRange := &index.PathRange{
				Lower: task.Range.Lower,
				Upper: task.Range.Upper,
			}
			id, err := d.storage.Compact(ctx, ids, defaultTTL, index.WithRange(pathRange))
			if err != nil {
				return nil, err
			}
			return serializeCompactionResult(&pfs.CompactionResult{
				Id: *id,
			})
		})
	}, backoff.NewInfiniteBackOff(), func(err error, _ time.Duration) error {
		log.Printf("error in compaction worker: %v", err)
		return nil
	})
	// Never ending backoff should prevent us from getting here.
	panic(err)
}

func serializeCompactionTask(task *pfs.CompactionTask) (*types.Any, error) {
	data, err := proto.Marshal(task)
	if err != nil {
		return nil, err
	}
	return &types.Any{
		TypeUrl: "/" + proto.MessageName(task),
		Value:   data,
	}, nil
}

func deserializeCompactionTask(taskAny *types.Any) (*pfs.CompactionTask, error) {
	task := &pfs.CompactionTask{}
	if err := types.UnmarshalAny(taskAny, task); err != nil {
		return nil, err
	}
	return task, nil
}

func serializeCompactionResult(res *pfs.CompactionResult) (*types.Any, error) {
	data, err := proto.Marshal(res)
	if err != nil {
		return nil, err
	}
	return &types.Any{
		TypeUrl: "/" + proto.MessageName(res),
		Value:   data,
	}, nil
}

func deserializeCompactionResult(any *types.Any) (*pfs.CompactionResult, error) {
	res := &pfs.CompactionResult{}
	if err := types.UnmarshalAny(any, res); err != nil {
		return nil, err
	}
	return res, nil
}