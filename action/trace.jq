def clean: tostring | gsub("[[:cntrl:]]+"; " ") | .[:200];
def target: if .name == "Read" then [.input.file_path]
  elif .name == "Grep" then [.input.pattern, .input.path]
  elif .name == "Glob" then [.input.pattern]
  else [] end;
fromjson? | objects
| if .type == "assistant" then
    .message.content[]? | select(.type == "tool_use")
    | ([.name] + (target | map(select(. != null) | clean))) | "» " + (map(clean) | join(" "))
  elif .type == "result" then
    "» done: turns=\(.num_turns | clean) duration=\((.duration_ms // 0) / 1000 | floor)s cost=$\(.total_cost_usd | clean) tokens in=\(.usage.input_tokens // 0) cache_read=\(.usage.cache_read_input_tokens // 0) cache_write=\(.usage.cache_creation_input_tokens // 0) out=\(.usage.output_tokens // 0)"
  else empty end
