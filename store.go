package main

import (
	"fmt"
	"time"

	"github.com/go-redis/redis"
)

var (
	ErrStoreSetValue    = fmt.Errorf("ErrStoreSetValue")
	ErrStoreKeyNotExist = fmt.Errorf("ErrStoreKeyNotExist")
)

type DataStore interface {
	Set(key string, value string) (err error)
	Get(key string) (value string, err error)
	Expire(key string, ttlSeconds int) (err error)
	Delete(key string) (err error)
	HSet(key string, field string, value string) (bool, error)
	HGet(key string, field string) (string, error)
	HGetAll(key string) (map[string]string, error)
	Keys(pattern string) ([]string, error)
}

type RedisDataStore struct {
	client *redis.Client
	prefix string
}

var _ DataStore = &RedisDataStore{}

func NewRedisDataStore(client *redis.Client, prefix string) DataStore {
	return &RedisDataStore{
		client: client,
		prefix: prefix,
	}
}

func (s *RedisDataStore) fmtKey(key string) string {
	return fmt.Sprintf("%s:%s", s.prefix, key)
}

func (s *RedisDataStore) extractKey(key string) string {
  return key[len(s.prefix)+1:]
}

func (s *RedisDataStore) Set(key string, value string) (err error) {
	var cmd *redis.StatusCmd
	key = s.fmtKey(key)
	cmd = s.client.Set(key, value, 0)
	err = cmd.Err()
	return
}

func (s *RedisDataStore) Expire(key string, ttl int) (err error) {
	var cmd *redis.BoolCmd
	key = s.fmtKey(key)
	cmd = s.client.Expire(key, time.Duration(ttl)*time.Second)
	err = cmd.Err()
	return
}

func (s *RedisDataStore) Get(key string) (value string, err error) {
	key = s.fmtKey(key)
	var cmd = s.client.Get(key)
	var bytesValue []byte
	bytesValue, err = cmd.Bytes()
	if err != nil {
		return
	}
	if bytesValue == nil {
		err = ErrStoreKeyNotExist
		return
	}
	value = string(bytesValue)
	return
}

func (s *RedisDataStore) Delete(key string) (err error) {
	key = s.fmtKey(key)
	var cmd = s.client.Del(key)
	err = cmd.Err()
	return
}

func (s *RedisDataStore) HSet(key string, field string, value string) (bool, error) {
	key = s.fmtKey(key)
	var cmd = s.client.HSet(key, field, value)
	return cmd.Result()
}

func (s *RedisDataStore) HGetAll(key string) (map[string]string, error) {
	key = s.fmtKey(key)
	var cmd = s.client.HGetAll(key)
	return cmd.Result()
}

func (s *RedisDataStore) HGet(key string, field string) (string, error) {
	key = s.fmtKey(key)
	var cmd = s.client.HGet(key, field)
	return cmd.Result()
}

func (s *RedisDataStore) Keys(pattern string) (keys []string, err error) {
	pattern = s.fmtKey(pattern)
	var cmd = s.client.Keys(pattern)
  keys, err = cmd.Result()
  for i := 0; i< len(keys); i++ {
    keys[i] = s.extractKey(keys[i])
  }
  return
}
