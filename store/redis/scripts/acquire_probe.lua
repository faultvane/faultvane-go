local now = now_us()
local s = load(KEYS[1])
local granted = 0
if s.st == 1 and now >= s.ts + s.od then
  transition(s, 2, now, 0)
end
if s.st == 2 then
  redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', number(now))
  if redis.call('ZCARD', KEYS[3]) < tonumber(ARGV[2]) then
    redis.call('ZADD', KEYS[3], number(now + tonumber(ARGV[3])), ARGV[1])
    granted = 1
  end
  s.fp = ARGV[5]
  save(s, ARGV[4])
end
return reply(s, now, granted)
