# Restricts each proposal's anchor to the head-side lines of the diff's hunks, one anyOf entry per file.
# Input: result.schema.json. $hunks: [{file, start, end}].
.properties.proposals.items.properties.anchor |= {
  description,
  anyOf: [$hunks | group_by(.file)[] | {
    type: "object",
    properties: {
      file: {const: .[0].file},
      line: {anyOf: [.[] | {type: "integer", minimum: .start, maximum: .end}]}
    },
    required: ["file", "line"],
    additionalProperties: false
  }]
}
