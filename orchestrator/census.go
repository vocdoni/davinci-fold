package orchestrator

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	iden3utils "github.com/iden3/go-iden3-crypto/utils"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	leanimt "github.com/vocdoni/lean-imt-go"
)

// maxCensusDepth is the longest lean-IMT path the batch guest accepts
// (circuit/src/census.rs MAX_CENSUS_DEPTH).
const maxCensusDepth = 61

// checkCensusOrigin accepts the census origins whose proofs ingest can verify:
// the lean-IMT Merkle origins (1 to 3).
func checkCensusOrigin(origin uint64) error {
	co := davinci.CensusOrigin(origin)
	switch {
	case co.IsMerkle():
		return nil
	case co.IsCSP():
		return fmt.Errorf("census origin %d (CSP) is not supported", origin)
	default:
		return fmt.Errorf("unknown census origin %d", origin)
	}
}

// censusWeightBits is the width of the weight in a census leaf,
// PackAddressWeight(address, weight) = address << 88 | weight.
const censusWeightBits = 88

// verifyCensusProof checks a lean-IMT membership proof the way the batch guest
// does (circuit/src/census.rs), so a vote that passes here cannot fail the
// batch on its census proof: at most maxCensusDepth siblings and no path bit
// above them, the proof's root equal to the election's census root, the leaf
// bound to address (davinci.LeafAddress, the bits the guest compares with the
// ballot proof's address) and the Poseidon walk from the leaf reaching the
// root. Every value must be 32-byte hex, as the prover parses it, and a
// BN254 field element. It returns the weight the leaf carries (its low 88
// bits, as the guest reads it) and the proof with every value re-encoded as
// 0x-prefixed lowercase hex.
func verifyCensusProof(censusRoot *big.Int, address []byte, cp davinci.CensusProof) (*big.Int, davinci.CensusProof, error) {
	var canonical davinci.CensusProof
	depth := len(cp.Siblings)
	if depth > maxCensusDepth {
		return nil, canonical, fmt.Errorf("census proof depth %d exceeds %d", depth, maxCensusDepth)
	}
	if cp.Index>>uint(depth) != 0 {
		return nil, canonical, fmt.Errorf("census proof has path bits above depth %d", depth)
	}
	root, err := parseFr(cp.Root)
	if err != nil {
		return nil, canonical, fmt.Errorf("census root: %w", err)
	}
	if root.Cmp(censusRoot) != 0 {
		return nil, canonical, fmt.Errorf("census root mismatch")
	}
	leaf, err := parseFr(cp.Leaf)
	if err != nil {
		return nil, canonical, fmt.Errorf("census leaf: %w", err)
	}
	if len(address) != common.AddressLength {
		return nil, canonical, fmt.Errorf("address must be %d bytes", common.AddressLength)
	}
	if leafAddr := davinci.LeafAddress(frLimbs(leaf)); !bytes.Equal(leafAddr[:], address) {
		return nil, canonical, fmt.Errorf("census leaf is for address %x, not %x", leafAddr, address)
	}
	siblings := make([]*big.Int, depth)
	canonical.Siblings = make([]string, depth)
	for i, s := range cp.Siblings {
		if siblings[i], err = parseFr(s); err != nil {
			return nil, canonical, fmt.Errorf("census sibling %d: %w", i, err)
		}
		canonical.Siblings[i] = frHex(siblings[i])
	}
	proof := leanimt.MerkleProof[*big.Int]{Root: root, Leaf: leaf, PathBits: cp.Index, Siblings: siblings}
	if !leanimt.VerifyProofWith(proof, leanimt.PoseidonHasher, leanimt.BigIntEqual) {
		return nil, canonical, fmt.Errorf("census proof does not reach the census root")
	}
	canonical.Root, canonical.Leaf, canonical.Index = frHex(root), frHex(leaf), cp.Index
	weight := new(big.Int).And(leaf, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), censusWeightBits), big.NewInt(1)))
	return weight, canonical, nil
}

// parseFr parses a 32-byte big-endian hex value (0x optional) that must be a
// BN254 scalar field element.
func parseFr(s string) (*big.Int, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid hex %q: %w", s, err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("want 32 bytes, got %d", len(b))
	}
	v := new(big.Int).SetBytes(b)
	if !iden3utils.CheckBigIntInField(v) {
		return nil, fmt.Errorf("%s is not a BN254 field element", s)
	}
	return v, nil
}

// frHex encodes a field element as 0x-prefixed 32-byte big-endian hex.
func frHex(v *big.Int) string {
	var b [32]byte
	v.FillBytes(b[:])
	return "0x" + hex.EncodeToString(b[:])
}

// frLimbs splits a field element into the guest's four little-endian u64
// limbs.
func frLimbs(v *big.Int) [4]uint64 {
	var b [32]byte
	v.FillBytes(b[:])
	var l [4]uint64
	for i := range l {
		l[i] = binary.BigEndian.Uint64(b[24-8*i : 32-8*i])
	}
	return l
}
