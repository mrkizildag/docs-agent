# Prints {"file","start","end"} as one JSON line for each hunk of a unified git diff that has head-side lines.
BEGIN {
  split("a b t n v f r", names, " ")
  for (i = 1; i <= 7; i++) { cesc[names[i]] = sprintf("%c", i + 6) }
  cesc["\""] = "\""
  cesc["\\"] = "\\"
  for (i = 1; i < 32; i++) { ord[sprintf("%c", i)] = i }
  ord[sprintf("%c", 127)] = 127
}

# unquote decodes a path git wrote C-quoted, e.g. "b/a\"b.md", up to its closing quote.
function unquote(s,   out, i, c) {
  out = ""
  for (i = 2; i <= length(s); i++) {
    c = substr(s, i, 1)
    if (c == "\"") { break }
    if (c != "\\") { out = out c; continue }
    c = substr(s, ++i, 1)
    if (c ~ /[0-7]/) { out = out sprintf("%c", (c * 64) + (substr(s, i + 1, 1) * 8) + substr(s, i + 2, 1)); i += 2 }
    else { out = out cesc[c] }
  }
  return out
}

function json(s,   out, i, c) {
  out = ""
  for (i = 1; i <= length(s); i++) {
    c = substr(s, i, 1)
    if (c == "\\" || c == "\"") { out = out "\\" c }
    else if (c in ord) { out = out sprintf("\\u%04x", ord[c]) }
    else { out = out c }
  }
  return "\"" out "\""
}

/^@@ / && old <= 0 && new <= 0 {
  s = $0
  sub(/^@@ -[0-9]+/, "", s)
  old = 1
  if (substr(s, 1, 1) == ",") { sub(/^,/, "", s); old = s + 0 }
  sub(/^[0-9]* \+/, "", s)
  start = s + 0
  new = 1
  if (match(s, /^[0-9]+,/)) { new = substr(s, RLENGTH + 1) + 0 }
  if (new > 0 && file != "") { printf("{\"file\":%s,\"start\":%d,\"end\":%d}\n", json(file), start, start + new - 1) }
  next
}
old > 0 || new > 0 {
  c = substr($0, 1, 1)
  if (c == "+") { new-- } else if (c == "-") { old-- } else if (c != "\\") { old--; new-- }
  next
}
/^\+\+\+ / {
  file = substr($0, 5)
  if (substr(file, 1, 1) == "\"") { file = unquote(file) } else { sub(/\t.*$/, "", file) }
  if (file == "/dev/null") { file = "" } else { sub(/^b\//, "", file) }
}
