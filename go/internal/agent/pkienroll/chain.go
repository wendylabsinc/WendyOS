package pkienroll

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
)

// SplitLeafAndChain splits a PEM bundle into its first certificate and the
// remainder. pki-core answers an enrolment or a renewal with a leaf-first chain
// in one blob (root excluded), while the agent stores the leaf and the chain
// separately because its dialers concatenate them in that order.
//
// Lifted from go/internal/cli/commands/certrenew.go on
// origin/sem/wdy-2899-acme-enrollment, which already had to solve exactly this
// for the CLI's renewal pre-flight. Non-CERTIFICATE blocks are skipped rather
// than treated as chain material.
func SplitLeafAndChain(bundlePEM string) (leaf, chain string, err error) {
	rest := []byte(bundlePEM)
	var blocks []*pem.Block
	for {
		block, remainder := pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			blocks = append(blocks, block)
		}
		rest = remainder
	}
	if len(blocks) == 0 {
		return "", "", errors.New("certificate response carried no certificate")
	}
	var leafBuf, chainBuf bytes.Buffer
	if err := pem.Encode(&leafBuf, blocks[0]); err != nil {
		return "", "", fmt.Errorf("re-encoding issued leaf: %w", err)
	}
	for _, b := range blocks[1:] {
		if err := pem.Encode(&chainBuf, b); err != nil {
			return "", "", fmt.Errorf("re-encoding issued chain: %w", err)
		}
	}
	return leafBuf.String(), chainBuf.String(), nil
}
