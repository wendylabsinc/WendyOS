package liteenroll

import _ "embed"

// DefaultTimeURL is Sigstore's public RFC 3161 timestamp authority.
const DefaultTimeURL = "https://timestamp.sigstore.dev/api/v1/timestamp"

// The TSA root is pinned independently of HTTPS and timestamp responses. Source:
// https://github.com/sigstore/root-signing/blob/main/targets/trusted_root.json
// Certificate SHA-256: 2aca8fea5d3ce48b01cc77076293c280e6c23ffe44034757ee7833ca9f45d633
// Valid until 2035-04-06. Root rotation requires updating this bundled anchor.
//
//go:embed sigstore-tsa-root.pem
var defaultTSARoots []byte
