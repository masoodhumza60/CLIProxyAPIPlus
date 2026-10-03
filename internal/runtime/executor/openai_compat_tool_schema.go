package executor

import (
	"encoding/json"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// normalizeCompatToolSchemas reduces the tool schemas in a translated request to
// a conservative JSON Schema subset.
//
// A provider configured under `openai-compatibility` has declared an
// OpenAI-shaped upstream, and the same shape is not universally accepted: several
// OpenAI-compatible servers reject a schema that carries a local `$ref`, an
// OpenAPI-only `nullable`, or a `type` union that includes null. Those
// rejections are schema-shaped rather than model-shaped, so they are fixed once
// here instead of per provider.
//
// The rewrite is semantically neutral: it inlines a reference and then removes
// the block it came from, drops an annotation that duplicates what a null type
// already says, and collapses a union that is only `[T, null]` down to `T`. A
// genuine union of two real types is left alone, because flattening it would
// silently discard a constraint the caller declared.
//
// A payload it cannot understand is returned untouched: guessing at a malformed
// body would be worse than forwarding what the caller wrote and letting the
// upstream answer.
func normalizeCompatToolSchemas(payload []byte) []byte {
	var request struct {
		Tools []any `json:"tools"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		return payload
	}
	if len(request.Tools) == 0 {
		return payload
	}

	normalized, err := json.Marshal(helps.NormalizeToolSchemas(request.Tools))
	if err != nil {
		// A schema that cannot be re-encoded is left as the caller wrote it,
		// because a mangled tool definition would fail at the model instead of
		// here where the cause is obvious.
		log.WithError(err).Debug("openai-compat: normalized tool schemas could not be encoded")
		return payload
	}
	// The existing array is removed before the replacement is written. Setting a
	// raw array over a path that already holds one splices the two together and
	// yields a body that is no longer valid JSON, so a request would be rejected
	// for a reason that has nothing to do with the caller's tools.
	cleared, errDelete := sjson.DeleteBytes(payload, "tools")
	if errDelete != nil {
		log.WithError(errDelete).Debug("openai-compat: could not clear the existing tool list")
		return payload
	}
	updated, errSet := sjson.SetRawBytes(cleared, "tools", normalized)
	if errSet != nil {
		log.WithError(errSet).Debug("openai-compat: could not write the normalized tool schemas back")
		return payload
	}
	return updated
}
