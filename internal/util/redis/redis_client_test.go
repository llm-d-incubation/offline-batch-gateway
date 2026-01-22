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

// Test for the redis client utilities.

package redis_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	utls "github.com/llm-d-incubation/batch-gateway/internal/util/tls"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gredis "github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"
)

func TestRedisClient(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Redis Client Suite")
}

var (
	logger      klog.Logger
	redisUrl    string
	redisCaCert string
	minirds     *miniredis.Miniredis = nil
)

func init() {
	fmt.Println("Initializing redis client test")
	logger = klog.Background()
	redisUrl = os.Getenv("WX_REDIS_URL")
	redisCaCert = os.Getenv("WX_REDIS_CACERT_PATH")
}

var _ = BeforeSuite(func() {
	if redisUrl == "" {
		minirds = miniredis.RunT(GinkgoT())
		Expect(minirds).ToNot(BeNil())
		redisUrl = "redis://" + minirds.Addr()
	}
})

var _ = AfterSuite(func() {
})

var _ = Describe("Redis Client", func() {
	var rds *gredis.Client
	var err error

	BeforeEach(func() {
		cfg := &redis.RedisClientConfig{
			Url:         redisUrl,
			ServiceName: "test-service",
		}
		if redisCaCert != "" {
			cfg.EnableTLS = true
			cfg.Certificates = &utls.Certificates{
				CaCertFile: redisCaCert,
			}
		}
		rds, err = redis.NewRedisClient(context.Background(), cfg)
		Expect(err).To(BeNil())
	})

	AfterEach(func() {
		if rds != nil {
			rds.Close()
		}
	})

	It("should create a redis client", func() {
		fmt.Printf("Memory address of redis client: %p\n", rds)
		Expect(rds).NotTo(BeNil())
	})

	It("should set, get and delete a key", func() {
		_, err := rds.Set(context.Background(), "k1", "v1", -1).Result()
		Expect(err).To(BeNil())
		val, err := rds.Get(context.Background(), "k1").Result()
		Expect(err).To(BeNil())
		Expect(val).To(Equal("v1"))
		_, err = rds.Del(context.Background(), "k1").Result()
		Expect(err).To(BeNil())
	})

	It("should fail to create a redis client with invalid URL", func() {
		cfgInv := &redis.RedisClientConfig{
			Url:         "redis://invalid-url",
			ServiceName: "test-service",
		}
		rdsInv, errInv := redis.NewRedisClient(context.Background(), cfgInv)
		Expect(errInv).ToNot(BeNil())
		Expect(rdsInv).To(BeNil())
	})
})
