# Mirrors review.Proposal.Validate: index_entry is set iff section is empty (a new doc).
# Also mirrors basedocs' ".md only for a new doc" rule: a new doc's doc_path is a .md file under docs/.
# Input: result.schema.json.
.properties.proposals.items += {
  if: {properties: {section: {const: ""}}, required: ["section"]},
  then: {required: ["index_entry"], properties: {index_entry: {type: "string", minLength: 1}, doc_path: {pattern: "^docs/.*\\.md$"}}},
  else: {properties: {index_entry: {type: "string", maxLength: 0}}}
}
