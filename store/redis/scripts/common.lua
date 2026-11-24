local function number(n)
  return string.format('%.0f', n)
end

local function now_us()
  local t = redis.call('TIME')
  return t[1] * 1000000 + t[2]
end

local function load(key)
  local v = redis.call('HMGET', key, 'st', 'gen', 'ts', 'od', 'ps', 'fp')
  return {
    st = tonumber(v[1]) or 0,
    gen = tonumber(v[2]) or 0,
    ts = tonumber(v[3]) or 0,
    od = tonumber(v[4]) or 0,
    ps = tonumber(v[5]) or 0,
    fp = v[6] or '',
    previous = v[6] or ''
  }
end

local function save(s, ttl)
  redis.call('HSET', KEYS[1], 'v', 1, 'st', s.st, 'gen', number(s.gen), 'ts', number(s.ts), 'od', number(s.od), 'ps', s.ps, 'fp', s.fp)
  redis.call('PEXPIRE', KEYS[1], ttl)
  redis.call('PEXPIRE', KEYS[2], ttl)
  redis.call('PEXPIRE', KEYS[3], ttl)
end

local function transition(s, to, now, od)
  s.st = to
  s.gen = s.gen + 1
  s.ts = now
  s.od = od
  s.ps = 0
  redis.call('DEL', KEYS[2], KEYS[3])
end

local function reply(s, now, granted)
  return {s.st, number(s.gen), number(s.ts), number(s.od), number(now), granted or 0, s.previous}
end
