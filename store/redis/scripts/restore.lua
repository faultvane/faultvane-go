local now = now_us()
local s = load(KEYS[1])
if tonumber(ARGV[2]) > s.gen then
  s.st, s.gen, s.ts, s.od, s.ps = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4]), 0
  redis.call('DEL', KEYS[2], KEYS[3])
  save(s, ARGV[5])
end
return reply(s, now)
