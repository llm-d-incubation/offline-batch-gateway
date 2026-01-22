package redis

import (
	"context"
	"flag"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.ibm.com/WML/utils-lib-go/pkg/logging"
	db_api "github.ibm.com/WML/watsonx-inference-proxy/internal/batch/database/api"
	"github.ibm.com/WML/watsonx-inference-proxy/internal/config"
	tredis "github.ibm.com/WML/watsonx-inference-proxy/tests/redis"
)

const (
	ServiceName = "test-service"
)

var (
	RedisUrlEnv                        = os.Getenv("TEST_REDIS_URL")
	RedisUrlArg                        = flag.String("redis_url", "", "redis url")
	RedisUrl                           = ""
	RedisCacertEnv                     = os.Getenv("TEST_REDIS_CACERT")
	RedisCacertArg                     = flag.String("redis_cacert", "", "redis cacert")
	RedisCacert                        = ""
	logger                             = logging.GetInstance()
	dbClient       *BatchDBClientRedis = nil
	serr           error               = nil
	lredis         *tredis.Localredis  = nil
	testTagVal1    string              = "test-tag-1"
	testTagVal2    string              = "test-tag-2"
	tagVal1        string              = "dif-tag-1"
	tagVal2        string              = "dif-tag-2"
	tagVal3        string              = "dif-tag-3"
)

func TestBatchDbRedis(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Batch DB Redis Suite")
}

var _ = BeforeSuite(func(ctx context.Context) {
	if *RedisUrlArg != "" {
		RedisUrl = *RedisUrlArg
	} else {
		RedisUrl = RedisUrlEnv
	}
	if *RedisCacertArg != "" {
		RedisCacert = *RedisCacertArg
	} else {
		RedisCacert = RedisCacertEnv
	}
	batchConf := &config.BatchConfig{
		Enabled:         true,
		Type:            config.BatchDBTypeRedis,
		RedisClientConf: &config.RedisClientConfig{Timeout: time.Second * 1},
	}
	if len(RedisUrl) == 0 {
		lredis, serr = tredis.Start()
		if serr == nil && lredis != nil {
			RedisUrl = config.REDIS_URL_LOCAL_UNSECURE_SCHEME + lredis.Addr()
		}
	}
	if len(RedisUrl) > 0 {
		batchConf.RedisClientConf.Fill(RedisUrl, "", "", RedisCacert, "")
		dbClient, serr = NewBatchDBClientRedis(batchConf, ServiceName, logger)
		Expect(serr).To(BeNil())
		Expect(dbClient).ToNot(BeNil())
	}
})

var _ = AfterSuite(func() {
	if dbClient != nil {
		serr = dbClient.Close()
		Expect(serr).To(BeNil())
	}
	if lredis != nil {
		lredis.Stop()
	}
})

var _ = Describe("Batch database using redis", Ordered, func() {

	Context("Basic tests", func() {

		PIt("should perform job operations concurrently", func() {
			if dbClient == nil {
				Skip("no redis")
			}

			nJobs := 40
			nJobsRmv := 10
			var wg sync.WaitGroup
			jobs := make(map[string]*db_api.BatchJob)
			for i := 0; i < nJobsRmv; i++ {
				jobID := uuid.New().String()
				job := &db_api.BatchJob{
					ID:     jobID,
					SLO:    time.Now().Add(time.Hour),
					TTL:    1,
					Tags:   []string{tagVal1, tagVal2},
					Spec:   []byte("spec"),
					Status: []byte("status"),
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					ID, err := dbClient.Store(context.Background(), logger, job)
					Expect(err).To(BeNil())
					Expect(ID).To(Equal(job.ID))
				}()
			}
			wg.Wait()
			for i := 0; i < nJobs; i++ {
				jobID := uuid.New().String()
				job := &db_api.BatchJob{
					ID:     jobID,
					SLO:    time.Now().Add(time.Hour),
					TTL:    10000,
					Tags:   []string{tagVal1, tagVal2},
					Spec:   []byte("spec"),
					Status: []byte("status"),
				}
				jobs[jobID] = job
				wg.Add(1)
				go func() {
					defer wg.Done()
					ID, err := dbClient.Store(context.Background(), logger, job)
					Expect(err).To(BeNil())
					Expect(ID).To(Equal(job.ID))
				}()
			}
			wg.Wait()
			time.Sleep(1 * time.Second) // To make sure the short ttl jobs get expired.

			resJobs, resEvents, err := dbClient.GetForProcessing(context.Background(), logger, time.Second*2, (nJobs+nJobsRmv)*2)
			Expect(err).To(BeNil())
			Expect(resJobs).To(HaveLen(nJobs))
			Expect(resEvents).To(HaveLen(nJobs))

			var sentEvents []db_api.BatchEvent
			for _, resJob := range resJobs {
				tJob := jobs[resJob.ID]
				Expect(tJob).ToNot(BeNil())
				Expect(resJob.ID).To(Equal(tJob.ID))
				Expect(resJob.SLO.Equal(tJob.SLO)).To(BeTrue())
				Expect(resJob.Spec).To(Equal(tJob.Spec))
				Expect(resJob.Status).To(Equal(tJob.Status))
				Expect(resJob.Tags).To(Equal(tJob.Tags))
				sentEvents = append(sentEvents,
					db_api.BatchEvent{ID: resJob.ID, Type: db_api.BatchEventPause, TTL: 1000})
			}

			sentIDs, err := dbClient.SendEvent(context.Background(), logger, sentEvents)
			Expect(err).To(BeNil())
			Expect(sentIDs).To(HaveLen(nJobs))
			for _, sentID := range sentIDs {
				_, found := jobs[sentID]
				Expect(found).To(BeTrue())
			}

			for _, resEvent := range resEvents {
				wg.Add(1)
				go func() {
					defer wg.Done()
					select {
					case event := <-resEvent.Events:
						Expect(event.ID).To(Equal(resEvent.ID))
						Expect(event.Type).To(BeNumerically("==", db_api.BatchEventPause))
					case <-time.After(4 * time.Second):
						Expect(false)
					}
					resEvent.CloseFn()
				}()
			}
			wg.Wait()

			var jobIDs []string
			for _, job := range jobs {
				jobIDs = append(jobIDs, job.ID)
				job.Tags = []string{tagVal1, tagVal3}
				job.Status = []byte("updated_status")
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := dbClient.Update(context.Background(), logger, job, false)
					Expect(err).To(BeNil())
					nJob := *job
					nJob.Tags = []string{}
					err = dbClient.Update(context.Background(), logger, &nJob, false)
					Expect(err).To(BeNil())
					nJob = *job
					nJob.Status = []byte{}
					err = dbClient.Update(context.Background(), logger, &nJob, false)
					Expect(err).To(BeNil())
					err = dbClient.Update(context.Background(), logger, job, true)
					Expect(err).To(BeNil())
				}()
			}
			wg.Wait()

			resJobs, _, err = dbClient.Get(context.Background(), logger, jobIDs, nil, db_api.TagsLogicalCondNa, true, 0, nJobs*2)
			Expect(err).To(BeNil())
			Expect(resJobs).To(HaveLen(nJobs))
			for _, resJob := range resJobs {
				tJob := jobs[resJob.ID]
				Expect(resJob.ID).To(Equal(tJob.ID))
				Expect(resJob.SLO.Equal(tJob.SLO)).To(BeTrue())
				Expect(resJob.Spec).To(Equal(tJob.Spec))
				Expect(resJob.Status).To(Equal(tJob.Status))
				Expect(resJob.Tags).To(Equal(tJob.Tags))
			}

			resJobs, _, err = dbClient.Get(context.Background(), logger, nil, nil, db_api.TagsLogicalCondNa, true, 0, nJobs*2)
			Expect(err).To(BeNil())
			Expect(resJobs).To(HaveLen(0))

			deletedIDs, err := dbClient.Delete(context.Background(), logger, jobIDs)
			Expect(err).To(BeNil())
			Expect(deletedIDs).To(HaveLen(nJobs))
			for _, deletedID := range deletedIDs {
				_, found := jobs[deletedID]
				Expect(found).To(BeTrue())
			}
		})

		It("should find jobs by tags", func() {
			if dbClient == nil {
				Skip("no redis")
			}

			nJobsT1, nJobsT2, nJobsT1T2 := 10, 10, 10
			var wg sync.WaitGroup
			jobs := make(map[string]*db_api.BatchJob)
			var jobIDs []string
			for i := 0; i < nJobsT1+nJobsT2+nJobsT1T2; i++ {
				jobID := uuid.New().String()
				var tags []string
				var pref string
				if i < nJobsT1 {
					tags = []string{testTagVal1}
					pref = "t1-"
				} else if i < nJobsT1+nJobsT2 {
					tags = []string{testTagVal2}
					pref = "t2-"
				} else {
					tags = []string{testTagVal1, testTagVal2}
					pref = "t1t2-"
				}
				job := &db_api.BatchJob{
					ID:     jobID,
					SLO:    time.Now().Add(time.Hour),
					TTL:    10000,
					Tags:   tags,
					Spec:   []byte("spec"),
					Status: []byte("status"),
				}
				jobs[pref+jobID] = job
				jobIDs = append(jobIDs, jobID)
				wg.Add(1)
				go func() {
					defer wg.Done()
					ID, err := dbClient.Store(context.Background(), logger, job)
					Expect(err).To(BeNil())
					Expect(ID).To(Equal(job.ID))
				}()
			}
			wg.Wait()

			var resJobs, curResJobs []*db_api.BatchJob
			var err error
			cursor, nextCursor := 0, 0
			for {
				curResJobs, nextCursor, err = dbClient.Get(context.Background(), logger, nil, []string{testTagVal1},
					db_api.TagsLogicalCondAnd, true, cursor, (nJobsT1+nJobsT1T2)*2)
				Expect(err).To(BeNil())
				if len(curResJobs) > 0 {
					resJobs = append(resJobs, curResJobs...)
				}
				if nextCursor == 0 {
					break
				} else {
					cursor = nextCursor
				}
			}
			Expect(resJobs).To(HaveLen(nJobsT1 + nJobsT1T2))
			for _, resJob := range resJobs {
				tJob, found := jobs["t1-"+resJob.ID]
				if !found {
					tJob, found = jobs["t1t2-"+resJob.ID]
				}
				Expect(found).To(BeTrue())
				Expect(resJob.ID).To(Equal(tJob.ID))
				Expect(resJob.SLO.Equal(tJob.SLO)).To(BeTrue())
				Expect(resJob.Tags).To(Equal(tJob.Tags))
				Expect(resJob.Status).To(Equal(tJob.Status))
				Expect(resJob.Spec).To(Equal(tJob.Spec))
			}

			resJobs = nil
			cursor, nextCursor = 0, 0
			for {
				curResJobs, nextCursor, err = dbClient.Get(context.Background(), logger, nil, []string{testTagVal2},
					db_api.TagsLogicalCondOr, false, cursor, (nJobsT2+nJobsT1T2)*2)
				Expect(err).To(BeNil())
				if len(curResJobs) > 0 {
					resJobs = append(resJobs, curResJobs...)
				}
				if nextCursor == 0 {
					break
				} else {
					cursor = nextCursor
				}
			}
			Expect(resJobs).To(HaveLen(nJobsT2 + nJobsT1T2))
			for _, resJob := range resJobs {
				tJob, found := jobs["t2-"+resJob.ID]
				if !found {
					tJob, found = jobs["t1t2-"+resJob.ID]
				}
				Expect(found).To(BeTrue())
				Expect(resJob.ID).To(Equal(tJob.ID))
				Expect(resJob.SLO.Equal(tJob.SLO)).To(BeTrue())
				Expect(resJob.Tags).To(Equal(tJob.Tags))
				Expect(resJob.Status).To(Equal(tJob.Status))
				Expect(resJob.Spec).To(BeEmpty())
			}

			resJobs = nil
			cursor, nextCursor = 0, 0
			for {
				curResJobs, nextCursor, err = dbClient.Get(context.Background(), logger, nil, []string{testTagVal1, testTagVal2},
					db_api.TagsLogicalCondAnd, false, cursor, nJobsT1T2*2)
				Expect(err).To(BeNil())
				if len(curResJobs) > 0 {
					resJobs = append(resJobs, curResJobs...)
				}
				if nextCursor == 0 {
					break
				} else {
					cursor = nextCursor
				}
			}
			Expect(resJobs).To(HaveLen(nJobsT1T2))
			for _, resJob := range resJobs {
				tJob, found := jobs["t1t2-"+resJob.ID]
				Expect(found).To(BeTrue())
				Expect(resJob.ID).To(Equal(tJob.ID))
				Expect(resJob.SLO.Equal(tJob.SLO)).To(BeTrue())
				Expect(resJob.Tags).To(Equal(tJob.Tags))
				Expect(resJob.Status).To(Equal(tJob.Status))
				Expect(resJob.Spec).To(BeEmpty())
			}

			resJobs = nil
			cursor, nextCursor = 0, 0
			for {
				curResJobs, nextCursor, err = dbClient.Get(context.Background(), logger, nil, []string{testTagVal1, testTagVal2},
					db_api.TagsLogicalCondOr, true, cursor, (nJobsT1+nJobsT2+nJobsT1T2)*2)
				Expect(err).To(BeNil())
				if len(curResJobs) > 0 {
					resJobs = append(resJobs, curResJobs...)
				}
				if nextCursor == 0 {
					break
				} else {
					cursor = nextCursor
				}
			}
			Expect(resJobs).To(HaveLen(nJobsT1 + nJobsT2 + nJobsT1T2))
			for _, resJob := range resJobs {
				tJob, found := jobs["t1t2-"+resJob.ID]
				if !found {
					tJob, found = jobs["t1-"+resJob.ID]
					if !found {
						tJob, found = jobs["t2-"+resJob.ID]
					}
				}
				Expect(found).To(BeTrue())
				Expect(resJob.ID).To(Equal(tJob.ID))
				Expect(resJob.SLO.Equal(tJob.SLO)).To(BeTrue())
				Expect(resJob.Tags).To(Equal(tJob.Tags))
				Expect(resJob.Status).To(Equal(tJob.Status))
				Expect(resJob.Spec).To(Equal(tJob.Spec))
			}

			deletedIDs, err := dbClient.Delete(context.Background(), logger, jobIDs)
			Expect(err).To(BeNil())
			Expect(deletedIDs).To(HaveLen(nJobsT1 + nJobsT2 + nJobsT1T2))
		})

		It("should reject invalid job objects", func() {
			if dbClient == nil {
				Skip("no redis")
			}

			job := &db_api.BatchJob{}
			ID, err := dbClient.Store(context.Background(), logger, job)
			Expect(err).ToNot(BeNil())
			Expect(ID).To(BeEmpty())

			job.ID = uuid.New().String()
			ID, err = dbClient.Store(context.Background(), logger, job)
			Expect(err).ToNot(BeNil())
			Expect(ID).To(BeEmpty())

			job.SLO = time.Now().Add(time.Hour)
			ID, err = dbClient.Store(context.Background(), logger, job)
			Expect(err).ToNot(BeNil())
			Expect(ID).To(BeEmpty())

			job.TTL = 20
			ID, err = dbClient.Store(context.Background(), logger, job)
			Expect(err).To(BeNil())
			Expect(ID).To(Equal(job.ID))

			resJobs, _, err := dbClient.Get(context.Background(), logger,
				[]string{job.ID}, nil, db_api.TagsLogicalCondNa, true, 0, 10)
			Expect(err).To(BeNil())
			Expect(resJobs).To(HaveLen(1))
			resJob := resJobs[0]
			Expect(resJob).ToNot(BeNil())
			Expect(resJob.ID).To(Equal(job.ID))
			Expect(resJob.Spec).To(BeEmpty())
			Expect(resJob.Status).To(BeEmpty())
			Expect(resJob.Tags).To(BeEmpty())

			deletedIDs, err := dbClient.Delete(context.Background(), logger, []string{job.ID})
			Expect(err).To(BeNil())
			Expect(deletedIDs).To(HaveLen(1))
			Expect(deletedIDs[0]).To(Equal(job.ID))
		})
	})
})
