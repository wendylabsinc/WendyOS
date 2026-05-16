package releasekeys

import _ "embed"

//go:embed wendy-releases.gpg.pub
var WendyReleasesPublicKey []byte
