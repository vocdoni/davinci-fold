// Package crypto provides the cryptographic helper functions used by
// davinci-fold. Vendored from davinci-node/crypto so this module owns its own
// signature-padding logic without pulling the full davinci-node crypto stack.
package crypto

// SignatureCircuitVariableLen is the standard size in bytes for serialized
// field elements passed to the signature circuit.
const SignatureCircuitVariableLen = 32

// PadToSign pads the input byte slice to SignatureCircuitVariableLen bytes:
// shorter inputs are left-padded with zeros, longer inputs are truncated to
// their last SignatureCircuitVariableLen bytes.
func PadToSign(input []byte) []byte {
	if len(input) < SignatureCircuitVariableLen {
		for len(input) < SignatureCircuitVariableLen {
			input = append([]byte{0}, input...)
		}
	} else if len(input) > SignatureCircuitVariableLen {
		input = input[len(input)-SignatureCircuitVariableLen:]
	}
	return input
}
