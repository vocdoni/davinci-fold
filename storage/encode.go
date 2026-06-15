package storage

import (
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// EncodeArtifact CBOR-encodes an artifact deterministically.
func EncodeArtifact(a any) ([]byte, error) {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		return nil, fmt.Errorf("encode artifact: %w", err)
	}
	return em.Marshal(a)
}

// DecodeArtifact CBOR-decodes into out.
func DecodeArtifact(data []byte, out any) error {
	return cbor.Unmarshal(data, out)
}
