// Package core is the direct Go incarnation of Wendy Core. It does not depend
// on the C bridge, JavaScript, or any SDK or platform adapter.
package core

// WendyHelloWorld returns 42. This temporary capability proves that packaged
// consumers reach the same engine. Removing it is a breaking contract change.
func WendyHelloWorld() int32 { return 42 }
