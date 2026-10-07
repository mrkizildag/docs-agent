# Prints a unified git diff with the head-side line number on every hunk line.
# Hunk lines must stay byte-identical to review.NumberedPatch.
/^@@ / && old <= 0 && new <= 0 {
  print
  s = $0
  sub(/^@@ -[0-9]+/, "", s)
  old = 1
  if (substr(s, 1, 1) == ",") { sub(/^,/, "", s); old = s + 0 }
  sub(/^[0-9]* \+/, "", s)
  n = s + 0
  new = 1
  if (match(s, /^[0-9]+,/)) { new = substr(s, RLENGTH + 1) + 0 }
  next
}
old <= 0 && new <= 0 { print; next }
/^\\/ { print; next }
$0 == "" { print; old--; new--; next }
{
  c = substr($0, 1, 1)
  text = substr($0, 2)
  if (c == "+") { printf("%6d +%s\n", n, text); n++; new-- }
  else if (c == "-") { printf("%6s -%s\n", "", text); old-- }
  else { printf("%6d  %s\n", n, text); n++; old--; new-- }
}
