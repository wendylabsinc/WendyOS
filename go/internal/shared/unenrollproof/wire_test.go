package unenrollproof

import (
	"crypto/x509/pkix"
	"encoding/asn1"
)

type basicResponse struct {
	Data         asn1.RawValue
	Algorithm    pkix.AlgorithmIdentifier
	Signature    asn1.BitString
	Certificates []asn1.RawValue `asn1:"optional,explicit,tag:0"`
}
type responseBytes struct {
	Type     asn1.ObjectIdentifier
	Response []byte
}
type wireResponse struct {
	Status asn1.Enumerated
	Bytes  responseBytes `asn1:"optional,explicit,tag:0"`
}

type envelope struct {
	Body      []byte `json:"body"`
	Signature []byte `json:"signature"`
}
