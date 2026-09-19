package record

import _ "embed" // the schema is part of the binary

// SchemaID is the "$id" of the v1 schema. It names the schema and is not promised to resolve:
// take the file from the repository, not from the network.
const SchemaID = "https://gablooge.github.io/sluiceway/schema/record/v1.json"

//go:embed record.v1.schema.json
var schemaV1 []byte

// Schema returns the JSON Schema (draft 2020-12) of format v1: the bytes of
// record.v1.schema.json, which is the file a sink author takes. The binary and the tests use
// these same bytes. The result is a copy.
//
// Validate with format assertion switched on where the validator has it. The schema also pins
// occurred_at with a pattern, for validators that treat "format" as an annotation.
func Schema() []byte {
	return append([]byte(nil), schemaV1...)
}
