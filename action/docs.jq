def list: if type == "array" and all(.[]; type == "string") then (if length == 0 then "(none)" else map("- " + tojson) | join("\n") end) else error("bad list") end;
def range_ok: type == "object" and (.start | type) == "number" and (.end | type) == "number";
def file_ok: type == "object" and (.path | type) == "string" and (.ranges | type) == "array" and all(.ranges[]; range_ok);
def norm:
  if type == "array" then {review: ., uncovered: [], base_sha: "", files: null}
  elif type == "object" then {review: (.review // []), uncovered: (.uncovered // []), base_sha: (.base_sha | if . == null then "" else . end), files: .files}
  else error("bad shape") end
  | if any(.review, .uncovered; type != "array" or (all(.[]; type == "string") | not)) then error("bad list") else . end
  | if (.base_sha | type) != "string" then error("bad base_sha") else . end
  | if .files != null and ((.files | type) != "array" or (all(.files[]; file_ok) | not)) then error("bad files") else . end;
norm
| if $key == "base_sha" then .base_sha
  elif $key == "has_files" then .files != null
  elif $key == "hunks" then (.files // [])[] | .path as $file | .ranges[] | {file: $file, start, end}
  else .[$key] | list end
