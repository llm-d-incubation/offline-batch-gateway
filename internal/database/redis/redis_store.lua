-- Store lua script.

-- Parse inputs.
local hashKey = KEYS[1]
local ssKey = KEYS[2]
local fieldVer = ARGV[1]
local fieldId = ARGV[2]
local fieldSlo = ARGV[3]
local fieldTags = ARGV[4]
local fieldStatus = ARGV[5]
local fieldSpec = ARGV[6]
local score = tonumber(ARGV[7])
local ttl = tonumber(ARGV[8])

-- Add the hash key.
redis.call('HSET', hashKey, "ver", fieldVer, "id", fieldId, "slo", fieldSlo, "tags", fieldTags, "status", fieldStatus, "spec", fieldSpec)

-- Set expiration.
local result = redis.pcall('EXPIRE', hashKey, ttl)
if type(result) == 'table' and result.err then
    redis.pcall('HDEL', hashKey, "ver", "id", "slo", "tags", "status", "spec")
    return result.err
end

-- Add the key to the sorted set.
result = redis.pcall('ZADD', ssKey, 'nx', score, fieldId)
if type(result) == 'table' and result.err then
    redis.pcall('HDEL', hashKey, "ver", "id", "slo", "tags", "status", "spec")
    return result.err
end

return ''
