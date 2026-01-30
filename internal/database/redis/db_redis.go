/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This file provides a redis database implementation.

package redis

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	db_api "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	goredis "github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"
)

const (
	fieldNameVersion     = "ver"
	fieldNameId          = "id"
	fieldNameSlo         = "slo"
	fieldNameSpec        = "spec"
	fieldNameStatus      = "status"
	fieldNameTags        = "tags"
	eventReadCount       = 4
	keysPrefix           = "wx_batch:"
	storeKeysPrefix      = keysPrefix + "store:"
	queueKeysPrefix      = keysPrefix + "queue:"
	eventKeysPrefix      = keysPrefix + "event:"
	priorityQueueKeyName = queueKeysPrefix + "priority"
	storeKeysPattern     = storeKeysPrefix + "*"
	routineStopTimeout   = 20 * time.Second
	eventChanTimeout     = 10 * time.Second
	tagsSep              = ";;"
	eventChanSize        = 100
	logFreqDefault       = 10 * time.Minute
	redisClientNameSuf   = "-batch"
	versionV1            = "1"
)

var (
	//go:embed redis_store.lua
	storeLua         string
	redisScriptStore = goredis.NewScript(storeLua)

	//go:embed redis_get_by_tags.lua
	getByTagsLua         string
	redisScriptGetByTags = goredis.NewScript(getByTagsLua)
)

type BatchDSClientRedis struct {
	redisClient        *goredis.Client
	redisClientChecker *uredis.RedisClientChecker
	timeout            time.Duration
	idleLogFreq        time.Duration
	idleLogLast        time.Time
}

func NewBatchDSClientRedis(ctx context.Context, conf *uredis.RedisClientConfig) (
	*BatchDSClientRedis, error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if conf == nil {
		err := fmt.Errorf("empty redis config")
		logger.Error(err, "NewBatchDSClientRedis:")
		return nil, err
	}
	redisClient, err := uredis.NewRedisClient(ctx, conf)
	if err != nil {
		return nil, err
	}
	redisClientChecker := uredis.NewRedisClientChecker(redisClient, keysPrefix, conf.ServiceName, conf.Timeout)
	logger.Info("NewBatchDSClientRedis: succeeded", "serviceName", conf.ServiceName)
	return &BatchDSClientRedis{
		redisClient:        redisClient,
		redisClientChecker: redisClientChecker,
		timeout:            conf.Timeout,
		idleLogFreq:        logFreqDefault,
		idleLogLast:        time.Now(),
	}, nil
}

func (c *BatchDSClientRedis) Close() (err error) {
	if c.redisClient != nil {
		err = c.redisClient.Close()
	}
	return err
}

func (c *BatchDSClientRedis) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	if timeLimit > 0 {
		return context.WithTimeout(parentCtx, timeLimit)
	}
	return context.WithTimeout(parentCtx, c.timeout)
}

func (c *BatchDSClientRedis) DBStore(ctx context.Context, item *db_api.BatchItem) (
	ID string, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if item == nil {
		err = fmt.Errorf("empty batch item")
		logger.Error(err, "Store:")
		return
	}
	if err = item.IsValid(); err != nil {
		logger.Error(err, "Store: item is invalid")
		return
	}
	logger = logger.WithValues("batchId", item.ID)

	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	res, err := redisScriptStore.Run(cctx, c.redisClient,
		[]string{getKeyForStore(item.ID)},
		versionV1, item.ID, strconv.FormatInt(item.SLO.UnixNano(), 10),
		packTags(item.Tags), item.Status, item.Spec,
		item.TTL).Text()
	ccancel()
	if err != nil {
		logger.Error(err, "Store: script failed")
		return "", err
	}
	if len(res) > 0 {
		err = fmt.Errorf("%s", res)
		logger.Error(err, "Store: script failed")
		return
	}
	logger.Info("Store: succeeded")
	return item.ID, nil
}

func (c *BatchDSClientRedis) DBUpdate(ctx context.Context, item *db_api.BatchItem) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if item == nil || len(item.ID) == 0 {
		err = fmt.Errorf("empty or invalid batch item")
		logger.Error(err, "Update:")
		return
	}
	logger = logger.WithValues("batchId", item.ID)
	if len(item.Status) == 0 && len(item.Tags) == 0 {
		logger.Info("Update: nothing to update")
		return
	}

	// Update the item in the database.
	updatedStatus, updatedTags := false, false
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	cmds, err := c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		switch {
		case len(item.Status) > 0 && len(item.Tags) > 0:
			pipe.HSet(cctx, getKeyForStore(item.ID),
				fieldNameStatus, item.Status, fieldNameTags, packTags(item.Tags)).Err()
			updatedStatus, updatedTags = true, true
		case len(item.Status) > 0:
			pipe.HSet(cctx, getKeyForStore(item.ID),
				fieldNameStatus, item.Status).Err()
			updatedStatus = true
		case len(item.Tags) > 0:
			pipe.HSet(cctx, getKeyForStore(item.ID),
				fieldNameTags, packTags(item.Tags)).Err()
			updatedTags = true
		}
		return nil
	})
	ccancel()
	if err != nil {
		logger.Error(err, "Update: Pipelined failed")
		return err
	}
	for _, cmd := range cmds {
		if err = cmd.Err(); err != nil {
			logger.Error(err, "Update: Command inside pipeline failed")
			return
		}
	}

	logger.Info("Update: succeeded", "updatedStatus", updatedStatus, "updatedTags", updatedTags)

	return
}

func (c *BatchDSClientRedis) DBDelete(ctx context.Context, IDs []string) (
	deletedIDs []string, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)

	// Delete the item records.
	resMap := make(map[string]*goredis.IntCmd)
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	cmds, err := c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
		for _, id := range IDs {
			res := pipe.HDel(cctx, getKeyForStore(id),
				fieldNameVersion, fieldNameId, fieldNameSlo, fieldNameTags, fieldNameStatus, fieldNameSpec)
			resMap[id] = res
		}
		return nil
	})
	ccancel()
	if err != nil {
		logger.Error(err, "Delete: Pipelined failed")
		return nil, err
	}
	for _, cmd := range cmds {
		if cmd.Err() != nil && cmd.Err() != goredis.Nil {
			err = cmd.Err()
			logger.Error(err, "Delete: Command inside pipeline failed")
			break
		}
	}
	deletedIDs = make([]string, 0, len(resMap))
	for id, res := range resMap {
		if res != nil && res.Err() == nil && res.Val() > 0 {
			deletedIDs = append(deletedIDs, id)
		}
	}

	logger.Info("Delete:", "nItems", len(deletedIDs), "IDs", deletedIDs)

	return
}

func (c *BatchDSClientRedis) DBGet(
	ctx context.Context, IDs []string, tags []string,
	tagsLogicalCond db_api.TagsLogicalCond, includeStatic bool, start, limit int) (
	items []*db_api.BatchItem, cursor int, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)

	if len(IDs) > 0 {

		// Get the item records.
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		cmds, err := c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
			for _, id := range IDs {
				if includeStatic {
					pipe.HMGet(cctx, getKeyForStore(id),
						fieldNameId, fieldNameSlo, fieldNameTags, fieldNameStatus, fieldNameSpec)
				} else {
					pipe.HMGet(cctx, getKeyForStore(id),
						fieldNameId, fieldNameSlo, fieldNameTags, fieldNameStatus)
				}
			}
			return nil
		})
		ccancel()
		if err != nil {
			logger.Error(err, "Get: Pipelined failed")
			return nil, 0, err
		}

		// Process the items.
		items = make([]*db_api.BatchItem, 0, len(cmds))
		for _, cmd := range cmds {
			if cmd.Err() != nil {
				if cmd.Err() != goredis.Nil {
					logger.Error(cmd.Err(), "Get: HMGet failed")
				}
				continue
			}
			hgetRes, ok := cmd.(*goredis.SliceCmd)
			if !ok {
				err := fmt.Errorf("unexpected result type from HMGet: %T", cmd)
				logger.Error(err, "Get:")
				return nil, 0, err
			}
			item, err := dbItemFromHget(hgetRes.Val(), includeStatic, logger)
			if err != nil {
				return nil, 0, err
			}
			if item != nil {
				items = append(items, item)
			}
		}
		cursor = len(items)

	} else if len(tags) > 0 {

		cond, found := db_api.TagsLogicalCondNames[tagsLogicalCond]
		if !found {
			err = fmt.Errorf("invalid logical condition value: %d", tagsLogicalCond)
			logger.Error(err, "Get:")
			return
		}
		var res []interface{}
		ctags := convertTags(tags)
		cctx, ccancel := context.WithTimeout(ctx, c.timeout)
		res, err = redisScriptGetByTags.Run(cctx, c.redisClient,
			ctags, strconv.FormatBool(includeStatic), storeKeysPattern, cond, start, limit).Slice()
		ccancel()
		if err != nil {
			logger.Error(err, "Get: script failed")
			return
		}
		if len(res) != 2 {
			err = fmt.Errorf("unexpected result from script")
			logger.Error(err, "Get:")
			return
		}
		resItems, ok := res[1].([]interface{})
		if !ok {
			err = fmt.Errorf("unexpected result type from script: %T", res[1])
			logger.Error(err, "Get:")
			return
		}
		resCursor, ok := res[0].(int64)
		if !ok {
			err = fmt.Errorf("unexpected result type from script: %T", res[0])
			logger.Error(err, "Get:")
			return
		}
		items = make([]*db_api.BatchItem, 0, len(resItems))
		for _, resItem := range resItems {
			item, err := dbItemFromHget(resItem.([]interface{}), includeStatic, logger)
			if err != nil {
				return nil, 0, err
			}
			if item != nil {
				items = append(items, item)
			}
		}
		cursor = int(resCursor)
	}

	logger.Info("Get: succeeded", "nItems", len(items))

	return
}

func getKeyForStore(key string) string {
	return storeKeysPrefix + key
}

func packTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return fmt.Sprintf("%s%s%s", tagsSep, strings.Join(tags, tagsSep), tagsSep)
}

func unpackTags(tags string) []string {
	if len(tags) == 0 {
		return nil
	}
	rTags := strings.Split(tags, tagsSep)
	if len(rTags) > 2 {
		return rTags[1 : len(rTags)-1]
	} else {
		return rTags
	}
}

func convertTags(tags []string) (ctags []string) {
	if len(tags) > 0 {
		ctags = make([]string, len(tags))
		for i, tag := range tags {
			ctags[i] = fmt.Sprintf("%s%s%s", tagsSep, tag, tagsSep)
		}
	}
	return
}

func dbItemFromHget(vals []interface{}, includeStatic bool, logger klog.Logger) (*db_api.BatchItem, error) {

	if (includeStatic && len(vals) != 5) || (!includeStatic && len(vals) != 4) {
		err := fmt.Errorf("unexpected result contents from HMGet: %v", vals)
		logger.Error(err, "dbItemFromHget:")
		return nil, err
	}
	var (
		id, slo, tags, status, spec string
		sloTime                     time.Time
		ok                          bool
	)
	id, ok = vals[0].(string)
	if !ok || len(id) == 0 {
		return nil, nil
	}
	slo, ok = vals[1].(string)
	if ok && len(slo) > 0 {
		sloNano, err := strconv.ParseInt(slo, 10, 64)
		if err != nil {
			logger.Error(err, "dbItemFromHget:")
			return nil, err
		}
		sloTime = time.Unix(0, sloNano)
	}
	tags, ok = vals[2].(string)
	if !ok {
		tags = ""
	}
	status, ok = vals[3].(string)
	if !ok {
		status = ""
	}
	if includeStatic {
		spec, ok = vals[4].(string)
		if !ok {
			spec = ""
		}
	}
	job := &db_api.BatchItem{
		ID:     id,
		SLO:    sloTime,
		Spec:   []byte(spec),
		Status: []byte(status),
		Tags:   unpackTags(tags),
	}
	return job, nil
}

func (c *BatchDSClientRedis) PQEnqueue(ctx context.Context, item *db_api.BatchJobPriority) (err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if item == nil {
		err = fmt.Errorf("empty item")
		logger.Error(err, "PQEnqueue:")
		return
	}
	if err = item.IsValid(); err != nil {
		logger.Error(err, "PQEnqueue: item is invalid")
		return
	}
	logger = logger.WithValues("batchId", item.ID)

	data, lerr := json.Marshal(item)
	if lerr != nil {
		err = lerr
		logger.Error(err, "PQEnqueue: Marshal failed")
		return

	}
	if err = item.IsValid(); err != nil {
		logger.Error(err, "PQEnqueue: validation failed")
		return
	}
	zitem := goredis.Z{
		Score:  float64(item.SLO.UnixMicro()),
		Member: data,
	}
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	res := c.redisClient.ZAddNX(cctx, priorityQueueKeyName, zitem)
	ccancel()
	if res == nil {
		err = fmt.Errorf("redis command result is nil")
		logger.Error(err, "PQEnqueue:")
		return
	}
	if err = res.Err(); err != nil {
		logger.Error(err, "PQEnqueue: redis ZAddNX failed")
		return
	}
	logger.Info("PQEnqueue: succeeded")
	return
}

func (c *BatchDSClientRedis) PQDelete(ctx context.Context, item *db_api.BatchJobPriority) (nDeleted int, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)
	if item == nil {
		err = fmt.Errorf("empty item")
		logger.Error(err, "PQDelete:")
		return
	}
	if err = item.IsValid(); err != nil {
		logger.Error(err, "PQDelete: item is invalid")
		return
	}
	logger = logger.WithValues("batchId", item.ID)

	if err = item.IsValid(); err != nil {
		logger.Error(err, "PQDelete: validation failed")
		return
	}
	score := strconv.FormatInt(item.SLO.UnixMicro(), 10)
	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
	res := c.redisClient.ZRemRangeByScore(cctx, priorityQueueKeyName, score, score)
	ccancel()
	if res == nil {
		err = fmt.Errorf("redis command result is nil")
		logger.Error(err, "PQDelete:")
		return
	}
	if err = res.Err(); err != nil {
		logger.Error(err, "PQDelete: redis ZRemRangeByScore failed")
		return
	}
	nDeleted = int(res.Val())
	logger.Info("PQDelete: succeeded")
	return
}

func (c *BatchDSClientRedis) PQDequeue(ctx context.Context, timeout time.Duration, maxObjs int) (
	jobPriorities []*db_api.BatchJobPriority, err error) {

	if ctx == nil {
		ctx = context.Background()
	}
	logger := klog.FromContext(ctx)

	// Get items from the queue.
	cctx, ccancel := context.WithTimeout(ctx, timeout+2*time.Second)
	_, vals, err := c.redisClient.BZMPop(
		cctx, timeout, goredis.Min.String(), int64(maxObjs), priorityQueueKeyName).Result()
	ccancel()
	if err != nil {
		if unrecognizedBlockingError(err) {
			logger.Error(err, "PQDequeue: BZMPop failed")
			cerr := c.redisClientChecker.Check(ctx)
			if cerr != nil {
				logger.Error(err, "PQDequeue: ClientCheck failed")
			}
			return nil, err
		}
		if time.Since(c.idleLogLast) >= c.idleLogFreq {
			logger.Info("PQDequeue: no items")
			c.idleLogLast = time.Now()
		}
		return nil, nil
	}
	if len(vals) == 0 {
		if time.Since(c.idleLogLast) >= c.idleLogFreq {
			logger.Info("PQDequeue: no jobs")
			c.idleLogLast = time.Now()
		}
		return nil, nil
	}

	jobPriorities = make([]*db_api.BatchJobPriority, 0, len(vals))
	for _, val := range vals {
		item := &db_api.BatchJobPriority{}
		err = json.Unmarshal([]byte(val.Member.(string)), item)
		if err != nil {
			logger.Error(err, "PQDelete: Unmarshal failed")
			return
		}
		jobPriorities = append(jobPriorities, item)
	}

	logger.Info("PQDequeue: succeeded")
	return
}

func unrecognizedBlockingError(err error) bool {
	errStr := err.Error()
	unrecognized :=
		err != goredis.Nil &&
			!strings.Contains(errStr, "i/o timeout") &&
			!strings.Contains(errStr, "context")
	return unrecognized
}

func (c *BatchDSClientRedis) ECConsumerGetChannel(ctx context.Context, ID string) (
	batchEventsChan *db_api.BatchEventsChan, err error) {

	return
}

// func (c *BatchDSClientRedis) GetForProcessing(
// 	ctx context.Context, timeout time.Duration, maxJobs int) (
// 	resJobs []*db_api.BatchJob, resEvents []*db_api.BatchEventsChan, err error) {

// 	// Get the job records.
// 	cctx, ccancel = context.WithTimeout(ctx, timeout)
// 	cmds, err := c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
// 		for _, val := range vals {
// 			pipe.HMGet(cctx, getKeyForStore(val.Member.(string)),
// 				fieldNameId, fieldNameSlo, fieldNameTags, fieldNameStatus, fieldNameSpec)
// 		}
// 		return nil
// 	})
// 	ccancel()
// 	if err != nil {
// 		logger.Error(err, "GetForProcessing: Pipelined failed")
// 		return nil, nil, err
// 	}

// 	// Process the jobs.
// 	for _, cmd := range cmds {

// 		if cmd.Err() != nil {
// 			if cmd.Err() != goredis.Nil {
// 				logger.Error(cmd.Err(), "GetForProcessing: HMGet failed")
// 			}
// 			continue
// 		}
// 		hgetRes, ok := cmd.(*goredis.SliceCmd)
// 		if !ok {
// 			err := fmt.Errorf("unexpected result type from HMGet: %T", cmd)
// 			logger.Error(err, "GetForProcessing:")
// 			return nil, nil, err
// 		}
// 		job, err := dbJobFromHget(hgetRes.Val(), true, logger)
// 		if err != nil {
// 			return nil, nil, err
// 		}
// 		if job != nil {
// 			// Add the job to the returned list.
// 			resJobs = append(resJobs, job)
// 			// Create the events listener for the job.
// 			lctx, lcancel := context.WithCancel(context.Background()) // Use a background context as this should be independent of the context of this call.
// 			llogger := logger.WithValues("jobID", job.ID)
// 			eventChan := make(chan db_api.BatchEvent, eventChanSize)
// 			stopChan := make(chan any, 1)
// 			closeFn := func() {
// 				llogger.Info("Listener: close start")
// 				lcancel() // Signal for listener termination.
// 				select {
// 				case <-stopChan: // Wait for listener termination, with a timeout.
// 				case <-time.After(routineStopTimeout):
// 				}
// 				llogger.Info("Listener: close end")
// 			}
// 			batchEventChan := &db_api.BatchEventsChan{
// 				ID:      job.ID,
// 				Events:  eventChan,
// 				CloseFn: closeFn,
// 			}
// 			resEvents = append(resEvents, batchEventChan)
// 			go func() {
// 				eventsKeyId := getKeyForEvent(job.ID)
// 				llogger.Info("Listener: start", "eventsKeyId", eventsKeyId)
// 				for {
// 					select {
// 					case <-lctx.Done():
// 						llogger.Info("Listener: received termination signal")
// 						close(eventChan)
// 						stopChan <- struct{}{}
// 						return
// 					default:
// 						llogger.Debug("Listener: Start BLMPop")
// 						lcctx, lccancel := context.WithTimeout(lctx, c.timeout+2*time.Second)
// 						_, events, err := c.redisClient.BLMPop(lcctx, c.timeout, "left", int64(eventReadCount), eventsKeyId).Result()
// 						lccancel()
// 						llogger.Debug("Listener: Finished BLMPop")
// 						if err != nil {
// 							if unrecognizedBlockingError(err) {
// 								llogger.Error(err, "Listener: BLMPop failed")
// 								cerr := c.redisClientChecker.Check(ctx, llogger)
// 								if cerr != nil {
// 									llogger.Error(err, "Listener: ClientCheck failed")
// 								}
// 							}
// 							continue
// 						}
// 						for _, event := range events {
// 							eventi, err := strconv.Atoi(event)
// 							if err != nil {
// 								llogger.Error(err, "Listener: strconv failed")
// 								continue
// 							}
// 							select {
// 							case eventChan <- db_api.BatchEvent{
// 								ID:   job.ID,
// 								Type: db_api.BatchEventType(eventi),
// 							}:
// 								llogger.Info("Listener: dispatched event", "type", event)
// 							case <-time.After(eventChanTimeout):
// 								llogger.Error(fmt.Errorf("couldn't send event"), "Listener:", "type", event)
// 							}
// 						}
// 					}
// 				}
// 			}()
// 		}
// 	}

// 	logger.Info("GetForProcessing: succeeded", "nJobs", len(resJobs))

// 	return
// }

// func (c *BatchDSClientRedis) SendEvent(ctx context.Context, events []db_api.BatchEvent) (
// 	sentIDs []string, err error) {

// 	// if logger == nil {
// 	// 	logger = logging.GetInstance()
// 	// }
// 	if len(events) == 0 {
// 		err = fmt.Errorf("empty events")
// 		logger.Error(err, "SendEvent:")
// 		return
// 	}
// 	for _, event := range events {
// 		if err = event.IsValid(); err != nil {
// 			logger.Error(err, "SendEvent: invalid event")
// 			return
// 		}
// 	}

// 	resMap := make(map[string]*goredis.IntCmd)
// 	cctx, ccancel := context.WithTimeout(ctx, c.timeout)
// 	_, err = c.redisClient.Pipelined(cctx, func(pipe goredis.Pipeliner) error {
// 		for _, event := range events {
// 			eventTypeStr := strconv.Itoa(int(event.Type))
// 			key := getKeyForEvent(event.ID)
// 			res := pipe.RPush(cctx, key, eventTypeStr)
// 			resMap[event.ID] = res
// 			pipe.Expire(cctx, key, time.Duration(int64(event.TTL)*int64(time.Second)))
// 		}
// 		return nil
// 	})
// 	ccancel()
// 	if err != nil {
// 		logger.Error(err, "SendEvent: Pipelined failed")
// 		return
// 	}
// 	for id, res := range resMap {
// 		if res != nil {
// 			if res.Err() == nil && res.Val() > 0 {
// 				sentIDs = append(sentIDs, id)
// 			} else if res.Err() != nil && err == nil {
// 				err = res.Err()
// 			}
// 		}
// 	}

// 	logger.Info("SendEvent:", "nJobs", len(sentIDs), "sentIDs", sentIDs)

// 	return
// }

// func getKeyForEvent(key string) string {
// 	return eventKeysPrefix + key
// }
