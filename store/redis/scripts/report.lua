local now = now_us()
local s = load(KEYS[1])
local epoch = math.floor(now / tonumber(ARGV[9]))
local oldest = epoch - tonumber(ARGV[10]) + 1
local sequence = tonumber(ARGV[15])
local applied_epoch, applied = string.match(sequence > 0 and redis.call('HGET', KEYS[4], ARGV[14]) or '', '^(%d+):(%d+)$')
local repeated = applied and tonumber(applied_epoch) >= oldest and tonumber(applied) >= sequence
if s.st == 0 and s.gen == tonumber(ARGV[1]) and not repeated then
  local calls, failures, slow = tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
  local all = redis.call('HGETALL', KEYS[2])
  for i = 1, #all, 2 do
    local bucket, kind = string.match(all[i], '^(%d+):(%a)$')
    if tonumber(bucket) < oldest then
      redis.call('HDEL', KEYS[2], all[i])
    elseif kind == 'c' then
      calls = calls + tonumber(all[i + 1])
    elseif kind == 'f' then
      failures = failures + tonumber(all[i + 1])
    else
      slow = slow + tonumber(all[i + 1])
    end
  end
  local field = number(epoch)
  if redis.call('HEXISTS', KEYS[2], field .. ':c') == 0 then
    local reporters = redis.call('HGETALL', KEYS[4])
    for i = 1, #reporters, 2 do
      if tonumber(string.match(reporters[i + 1], '^(%d+):')) < oldest then
        redis.call('HDEL', KEYS[4], reporters[i])
      end
    end
  end
  if sequence > 0 then
    redis.call('HSET', KEYS[4], ARGV[14], field .. ':' .. ARGV[15])
  end
  redis.call('HINCRBY', KEYS[2], field .. ':c', ARGV[2])
  redis.call('HINCRBY', KEYS[2], field .. ':f', ARGV[3])
  redis.call('HINCRBY', KEYS[2], field .. ':s', ARGV[4])
  local failure_threshold, slow_rate = tonumber(ARGV[5]), tonumber(ARGV[8])
  local exceeded = failure_threshold > 0 and failures >= failure_threshold
  if not exceeded and calls >= tonumber(ARGV[6]) then
    exceeded = failures >= tonumber(ARGV[7]) * calls or (slow_rate > 0 and slow >= slow_rate * calls)
  end
  if exceeded then
    transition(s, 1, now, tonumber(ARGV[11]))
  end
  s.fp = ARGV[13]
  save(s, ARGV[12])
end
return reply(s, now)
