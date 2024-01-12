package filer

import (
	"context"
	"github.com/redis/go-redis/v9"
	"strconv"
	"testing"
	"time"
)

func TestNewQuotaPluginProvider(t *testing.T) {
	cli := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:        []string{":7000", ":7001", ":7002", ":7003", ":7004", ":7005"},
		MaxRetries:   5,
		DialTimeout:  10 * time.Second,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		PoolSize:     15,
		PoolTimeout:  30 * time.Second,
	})

	err := cli.Ping(context.Background()).Err()
	if err != nil {
		t.Fatal(err)
	}

	t.Log(cli.Get(context.Background(), "foo").Val())
	t.Log(cli.Get(context.Background(), "hello").Val())

	for i := 0; i < 100; i++ {

		is := strconv.Itoa(i)
		//t.Log("run " + is)
		//err = cli.Set(is, is, 0).Err()
		//if err != nil {
		//	t.Fatal(err)
		//}

		v, err := cli.Get(context.Background(), is).Result()
		if err != nil {
			t.Fatal(err)
		}
		if v != is {
			t.Fatal("not equal " + v)
		}
	}

}