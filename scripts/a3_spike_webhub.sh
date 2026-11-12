#!/usr/bin/env bash
# A3.0 spike: does glm-4.6 via WebHub support OpenAI tool calling with
# stream:true, and does it report usage? Results went into the PR #66
# description and docs/AGENT_PLAN_A3.md §5.
#
# This file contains NO key: the hub key always comes from the
# environment — export WEBHUB_KEY=<key> before running. WEBHUB_BASE
# and WEBHUB_MODEL are optional overrides.
set -u
BASE="${WEBHUB_BASE:-https://h11hp9dd1y90-d.space-z.ai/v1}"
KEY="${WEBHUB_KEY:?export WEBHUB_KEY=<hub key> before running}"
MODEL="${WEBHUB_MODEL:-glm-4.6}"

req() {
  curl -sS -N --max-time 60 -X POST "$BASE/chat/completions" \
    -H "Authorization: Bearer $KEY" \
    -H "Content-Type: application/json" \
    -H "X-WebHub-Memory: 0" \
    -d "$1"
}

echo "=== 1. tool calling + stream:true ==="
req '{
  "model": "'"$MODEL"'",
  "stream": true,
  "stream_options": {"include_usage": true},
  "tool_choice": "auto",
  "tools": [{
    "type": "function",
    "function": {
      "name": "get_weather",
      "description": "Get current weather for a city",
      "parameters": {
        "type": "object",
        "properties": {
          "city": {"type": "string"},
          "unit": {"type": "string", "enum": ["c", "f"]}
        },
        "required": ["city"]
      }
    }
  }],
  "messages": [
    {"role": "system", "content": "You are a helpful assistant. Use the get_weather tool when the user asks about weather."},
    {"role": "user", "content": "What is the weather in Hanoi right now?"}
  ]
}' | head -c 4000
echo
echo

echo "=== 2. parallel tool calls (2 tools, one turn) ==="
req '{
  "model": "'"$MODEL"'",
  "stream": true,
  "stream_options": {"include_usage": true},
  "tool_choice": "auto",
  "tools": [
    {"type": "function","function":{"name":"get_time","description":"Get current time for a timezone","parameters":{"type":"object","properties":{"tz":{"type":"string"}},"required":["tz"]}}},
    {"type": "function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}
  ],
  "messages": [
    {"role": "user", "content": "Tell me the time in Tokyo AND the weather in Osaka. Call both tools."}
  ]
}' | head -c 4000
echo
echo

echo "=== 3. plain text with tools present (no tool call expected) ==="
req '{
  "model": "'"$MODEL"'",
  "stream": true,
  "stream_options": {"include_usage": true},
  "tool_choice": "auto",
  "tools": [
    {"type": "function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}
  ],
  "messages": [
    {"role": "user", "content": "Say pong. Do not call any tool."}
  ]
}' | head -c 2500
echo
echo

echo "=== 4. tools param on a model that may reject (glm-4.5-air) ==="
curl -sS --max-time 60 -X POST "$BASE/chat/completions" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -H "X-WebHub-Memory: 0" \
  -o /dev/null -w "HTTP %{http_code}\n" -d '{
  "model": "glm-4.5-air",
  "stream": false,
  "tools": [{"type": "function","function":{"name":"t","description":"t","parameters":{"type":"object","properties":{}}}}],
  "messages": [{"role": "user", "content": "hi"}]
}'
echo

echo "=== 5. non-stream tool call (reference shape) ==="
req '{
  "model": "'"$MODEL"'",
  "stream": false,
  "tool_choice": "auto",
  "tools": [{"type": "function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
  "messages": [{"role": "user", "content": "Weather in Hanoi?"}]
}' | head -c 1500
echo
