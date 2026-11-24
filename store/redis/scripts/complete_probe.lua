local now = now_us()
local s = load(KEYS[1])
if s.st == 2 and s.gen == tonumber(ARGV[1]) then
  redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', number(now))
  if redis.call('ZREM', KEYS[3], ARGV[2]) == 1 then
    local result = tonumber(ARGV[3])
    if result == 1 then
      transition(s, 1, now, tonumber(ARGV[5]))
    elseif result == 0 then
      s.ps = s.ps + 1
      if s.ps >= tonumber(ARGV[4]) then
        transition(s, 0, now, 0)
      end
    end
  end
  save(s, ARGV[6])
end
return reply(s, now)
