local time = redis.call('TIME')
local now = time[1] * 1000000 + time[2]
local wanted = tonumber(ARGV[1])
local returned = tonumber(ARGV[2])
local rate = tonumber(ARGV[3])
local burst = tonumber(ARGV[4])
local bucket = redis.call('HMGET', KEYS[1], 'tk', 'ts')
local tokens = tonumber(bucket[1]) or burst
local since = tonumber(bucket[2]) or now
if now < since then
  now = since
end
tokens = math.min(burst, tokens + (now - since) * rate / 1000000 + returned)
local granted = math.max(math.floor(math.min(wanted, tokens)), 0)
redis.call('HSET', KEYS[1], 'v', 1, 'tk', string.format('%.17g', tokens - granted), 'ts', string.format('%.0f', now))
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return {string.format('%.0f', granted), string.format('%.0f', now)}
