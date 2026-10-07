# Mirrors review.Proposal.Validate: index_entry is set iff section is empty (a new doc).
# Input: result.schema.json.
.properties.proposals.items += {
  if: {properties: {section: {const: ""}}, required: ["section"]},
  then: {required: ["index_entry"], properties: {index_entry: {type: "string", minLength: 1}}},
  else: {properties: {index_entry: {type: "string", maxLength: 0}}}
}
