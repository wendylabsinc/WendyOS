// Package wendysim contains the allowlisted, source-free Wendy image templates.
package wendysim

import _ "embed"

//go:embed Dockerfile
var Dockerfile []byte

//go:embed .dockerignore
var Dockerignore []byte
