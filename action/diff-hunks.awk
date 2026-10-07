# Prints "path<TAB>start<TAB>end" for each hunk of a unified git diff that has head-side lines.
/^@@ / && old <= 0 && new <= 0 {
  s = $0
  sub(/^@@ -[0-9]+/, "", s)
  old = 1
  if (substr(s, 1, 1) == ",") { sub(/^,/, "", s); old = s + 0 }
  sub(/^[0-9]* \+/, "", s)
  start = s + 0
  new = 1
  if (match(s, /^[0-9]+,/)) { new = substr(s, RLENGTH + 1) + 0 }
  if (new > 0 && file != "") { printf("%s\t%d\t%d\n", file, start, start + new - 1) }
  next
}
old > 0 || new > 0 {
  c = substr($0, 1, 1)
  if (c == "+") { new-- } else if (c == "-") { old-- } else if (c != "\\") { old--; new-- }
  next
}
/^\+\+\+ / {
  file = substr($0, 5)
  sub(/\t.*$/, "", file)
  if (file == "/dev/null" || file ~ /["\\]|[[:cntrl:]]/) { file = "" } else { sub(/^b\//, "", file) }
}
