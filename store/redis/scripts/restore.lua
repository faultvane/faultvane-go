local now = now_us()
local s = load(KEYS[1])
if tonumber(ARGV[2]) > s.gen then
  s = {st = tonumber(ARGV[1]), gen = tonumber(ARGV[2]), ts = tonumber(ARGV[3]), od = tonumber(ARGV[4]), ps = 0}
  redis.call('DEL', KEYS[2], KEYS[3])
  save(s, ARGV[5])
end
return reply(s, now)
