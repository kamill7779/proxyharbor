package selector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/redis/go-redis/v9"
)

const zfairSelectScript = `
local ready = KEYS[1]
local delayed = KEYS[2]
local node_prefix = KEYS[3]
local now_ms = tonumber(ARGV[1])
local quantum = tonumber(ARGV[2])
local default_latency_ms = tonumber(ARGV[3])
local max_promote = tonumber(ARGV[4])
local max_scan = tonumber(ARGV[5])
local allowed = {}
for i = 6, #ARGV do
  allowed[ARGV[i]] = true
end

for proxy_id, _ in pairs(allowed) do
  local node_key = node_prefix .. proxy_id
  if redis.call('EXISTS', node_key) == 1 and redis.call('ZSCORE', ready, proxy_id) == false and redis.call('ZSCORE', delayed, proxy_id) == false then
    local node = redis.call('HMGET', node_key, 'weight', 'health_score', 'circuit_open_until', 'next_eligible_at', 'virtual_runtime')
    local weight = tonumber(node[1]) or 0
    local health_score = tonumber(node[2]) or 0
    local circuit_open_until = tonumber(node[3]) or 0
    local next_eligible_at = tonumber(node[4]) or 0
    local vr = tonumber(node[5]) or 0
    if weight > 0 and health_score > 0 then
      if circuit_open_until > now_ms then
        redis.call('ZADD', delayed, circuit_open_until, proxy_id)
      elseif next_eligible_at > now_ms then
        redis.call('ZADD', delayed, next_eligible_at, proxy_id)
      else
        redis.call('ZADD', ready, vr, proxy_id)
      end
    end
  end
end

local due = redis.call('ZRANGEBYSCORE', delayed, '-inf', now_ms, 'LIMIT', 0, max_promote)
for _, proxy_id in ipairs(due) do
  redis.call('ZREM', delayed, proxy_id)
  local node_key = node_prefix .. proxy_id
  local vr = tonumber(redis.call('HGET', node_key, 'virtual_runtime') or '0')
  if allowed[proxy_id] and redis.call('EXISTS', node_key) == 1 then
    redis.call('ZADD', ready, vr, proxy_id)
  end
end

if redis.call('ZCARD', ready) == 0 then
  for proxy_id, _ in pairs(allowed) do
    local node_key = node_prefix .. proxy_id
    if redis.call('EXISTS', node_key) == 1 and redis.call('ZSCORE', delayed, proxy_id) == false then
      local node = redis.call('HMGET', node_key, 'weight', 'health_score', 'circuit_open_until', 'next_eligible_at', 'virtual_runtime')
      local weight = tonumber(node[1]) or 0
      local health_score = tonumber(node[2]) or 0
      local circuit_open_until = tonumber(node[3]) or 0
      local next_eligible_at = tonumber(node[4]) or 0
      local vr = tonumber(node[5]) or 0
      if weight > 0 and health_score > 0 then
        if circuit_open_until > now_ms then
          redis.call('ZADD', delayed, circuit_open_until, proxy_id)
        elseif next_eligible_at > now_ms then
          redis.call('ZADD', delayed, next_eligible_at, proxy_id)
        else
          redis.call('ZADD', ready, vr, proxy_id)
        end
      end
    end
  end
end

for i = 1, max_scan do
  local picked = redis.call('ZRANGE', ready, 0, 0, 'WITHSCORES')
  if #picked == 0 then
    return nil
  end

  local proxy_id = picked[1]
  local current_vr = tonumber(picked[2]) or 0
  redis.call('ZREM', ready, proxy_id)
  if not allowed[proxy_id] then
    redis.call('DEL', node_prefix .. proxy_id)
  else
    local node_key = node_prefix .. proxy_id
    local node = redis.call('HMGET', node_key, 'weight', 'health_score', 'latency_ewma_ms', 'circuit_open_until', 'next_eligible_at')
    local weight = tonumber(node[1]) or 0
    local health_score = tonumber(node[2]) or 0
    local latency_ms = tonumber(node[3]) or default_latency_ms
    local circuit_open_until = tonumber(node[4]) or 0
    local next_eligible_at = tonumber(node[5]) or 0

    if weight <= 0 or health_score <= 0 then
      redis.call('DEL', node_key)
    elseif circuit_open_until > now_ms then
      redis.call('ZADD', delayed, circuit_open_until, proxy_id)
    elseif next_eligible_at > now_ms then
      redis.call('ZADD', delayed, next_eligible_at, proxy_id)
    else
      local health_factor = health_score / 100
      local latency_factor = 1 / (1 + latency_ms / 1000)
      local effective_weight = weight * health_factor * latency_factor
      if effective_weight <= 0 then
        effective_weight = 1
      end
      local new_vr = current_vr + quantum / effective_weight
      redis.call('HSET', node_key, 'virtual_runtime', new_vr, 'next_eligible_at', now_ms)
      redis.call('ZADD', ready, new_vr, proxy_id)
      return {proxy_id, tostring(new_vr), tostring(effective_weight), tostring(now_ms)}
    end
  end
end

return nil
`

type RedisZFair struct {
	client           *redis.Client
	now              func() time.Time
	quantum          float64
	defaultLatencyMS int64
	maxPromote       int64
	maxScan          int64
}

type RedisZFairConfig struct {
	Addr             string
	Password         string
	DB               int
	Quantum          float64
	DefaultLatencyMS int64
	MaxPromote       int64
	MaxScan          int64
}

func NewRedisZFair(ctx context.Context, cfg RedisZFairConfig) (*RedisZFair, error) {
	if cfg.Addr == "" {
		return nil, errors.New("zfair requires redis addr")
	}
	client := redis.NewClient(&redis.Options{Addr: cfg.Addr, Password: cfg.Password, DB: cfg.DB, DialTimeout: 3 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	if cfg.Quantum <= 0 {
		cfg.Quantum = 1000
	}
	if cfg.DefaultLatencyMS <= 0 {
		cfg.DefaultLatencyMS = 200
	}
	if cfg.MaxPromote <= 0 {
		cfg.MaxPromote = 128
	}
	if cfg.MaxScan <= 0 {
		cfg.MaxScan = 128
	}
	return &RedisZFair{client: client, now: time.Now, quantum: cfg.Quantum, defaultLatencyMS: cfg.DefaultLatencyMS, maxPromote: cfg.MaxPromote, maxScan: cfg.MaxScan}, nil
}

func (s *RedisZFair) Close() error { return s.client.Close() }

func (s *RedisZFair) Check(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return s.client.Ping(pingCtx).Err()
}

func (s *RedisZFair) Select(ctx context.Context, candidates []domain.Proxy, _ SelectOptions) (domain.Proxy, error) {
	if len(candidates) == 0 {
		return domain.Proxy{}, domain.ErrNoHealthyProxy
	}
	candidateByID := make(map[string]domain.Proxy, len(candidates))
	pipe := s.client.Pipeline()
	for _, candidate := range candidates {
		if !candidate.Healthy || candidate.Weight <= 0 || candidate.HealthScore <= 0 {
			continue
		}
		latencyMS := candidate.LatencyEWMAms
		if latencyMS <= 0 {
			latencyMS = int(s.defaultLatencyMS)
		}
		circuitOpenUntil := int64(0)
		if !candidate.CircuitOpenUntil.IsZero() {
			circuitOpenUntil = candidate.CircuitOpenUntil.UTC().UnixMilli()
		}
		candidateByID[candidate.ID] = candidate
		pipe.HSet(ctx, nodeKey(candidate.ID), map[string]any{
			"proxy_id":           candidate.ID,
			"weight":             candidate.Weight,
			"health_score":       candidate.HealthScore,
			"latency_ewma_ms":    latencyMS,
			"circuit_open_until": circuitOpenUntil,
		})
		pipe.HSetNX(ctx, nodeKey(candidate.ID), "virtual_runtime", 0)
		pipe.HSetNX(ctx, nodeKey(candidate.ID), "next_eligible_at", 0)
	}
	if len(candidateByID) == 0 {
		return domain.Proxy{}, domain.ErrNoHealthyProxy
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return domain.Proxy{}, err
	}

	keys := []string{readyKey(), delayedKey(), nodePrefix()}
	args := []any{s.now().UTC().UnixMilli(), s.quantum, s.defaultLatencyMS, s.maxPromote, s.maxScan}
	for proxyID := range candidateByID {
		args = append(args, proxyID)
	}
	result, err := s.client.Eval(ctx, zfairSelectScript, keys, args...).Result()
	if errors.Is(err, redis.Nil) || result == nil {
		if rebuildErr := s.ensureCandidates(ctx, candidateByID); rebuildErr != nil {
			return domain.Proxy{}, noHealthy(domain.ErrorKindSelectorReadyRebuild, "ready_empty_rebuild_failed", rebuildErr)
		}
		result, err = s.client.Eval(ctx, zfairSelectScript, keys, args...).Result()
		if errors.Is(err, redis.Nil) || result == nil {
			return domain.Proxy{}, noHealthy(domain.ErrorKindSelectorReadyRebuild, "ready_empty_after_rebuild", nil)
		}
	}
	if err != nil {
		return domain.Proxy{}, noHealthy(domain.ErrorKindSelectorRedis, "redis_eval_failed", err)
	}
	values, ok := result.([]any)
	if !ok || len(values) == 0 {
		return domain.Proxy{}, noHealthy(domain.ErrorKindSelectorMalformedResult, "malformed_result", nil)
	}
	proxyID := fmt.Sprint(values[0])
	selected, ok := candidateByID[proxyID]
	if !ok {
		return domain.Proxy{}, noHealthy(domain.ErrorKindSelectorStaleResult, "stale_result", nil)
	}
	return selected, nil
}

func (s *RedisZFair) ensureCandidates(ctx context.Context, candidates map[string]domain.Proxy) error {
	lockKey := rebuildLockKey()
	locked, err := s.client.SetNX(ctx, lockKey, "1", 5*time.Second).Result()
	if err != nil {
		return err
	}
	if !locked {
		return s.waitForRebuild(ctx)
	}
	defer s.client.Del(context.Background(), lockKey)

	pipe := s.client.Pipeline()
	nowMS := s.now().UTC().UnixMilli()
	for proxyID, candidate := range candidates {
		if !candidate.Healthy || candidate.Weight <= 0 || candidate.HealthScore <= 0 {
			continue
		}
		nodeKey := nodeKey(proxyID)
		score := float64(0)
		if vr, err := s.client.HGet(ctx, nodeKey, "virtual_runtime").Float64(); err == nil {
			score = vr
		}
		circuitOpenUntil := int64(0)
		if !candidate.CircuitOpenUntil.IsZero() {
			circuitOpenUntil = candidate.CircuitOpenUntil.UTC().UnixMilli()
		}
		if circuitOpenUntil > nowMS {
			pipe.ZAdd(ctx, delayedKey(), redis.Z{Score: float64(circuitOpenUntil), Member: proxyID})
			continue
		}
		pipe.ZAdd(ctx, readyKey(), redis.Z{Score: score, Member: proxyID})
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisZFair) waitForRebuild(ctx context.Context) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(250 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		case <-ticker.C:
			exists, err := s.client.Exists(ctx, rebuildLockKey()).Result()
			if err != nil {
				return err
			}
			if exists == 0 {
				return nil
			}
		}
	}
}

func readyKey() string              { return "proxyharbor:{global}:selector:ready" }
func delayedKey() string            { return "proxyharbor:{global}:selector:delayed" }
func rebuildLockKey() string        { return "proxyharbor:{global}:selector:rebuild_lock" }
func nodePrefix() string            { return "proxyharbor:{global}:selector:node:" }
func nodeKey(proxyID string) string { return nodePrefix() + proxyID }
