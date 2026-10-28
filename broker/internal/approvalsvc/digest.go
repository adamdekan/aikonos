package approvalsvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// ArgsDigest binds an approval to its arguments (AG-07, M-28; the deferred
// args_hash of ADR-006): sha256 (hex) of the canonical JSON array of the
// steps' args in the given (seq) order. Canonical = encoding/json's sorted map
// keys, no HTML escaping. Both ends compute it here — SubmitPlan hands it out
// and Resolve recomputes it from the stored args — so no client has to
// reproduce the encoding.
func ArgsDigest(steps []db.ExecutableStep) string {
	args := make([]any, len(steps))
	for i, s := range steps {
		if s.Args == nil {
			args[i] = map[string]any{}
		} else {
			args[i] = s.Args
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(args) // map[string]any decoded from JSON always encodes
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}
