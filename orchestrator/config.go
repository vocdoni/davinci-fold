package orchestrator

import (
	"fmt"
	"math/big"
	"strings"

	groth16 "github.com/consensys/gnark/backend/groth16/bn254"
	iden3utils "github.com/iden3/go-iden3-crypto/utils"
	"github.com/vocdoni/davinci-fold/types"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
	bjjgnark "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/spec"
	vtypes "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/types"
)

// ballotPublicInputs is the ballot proof's public signal count: address, vote
// ID and inputs hash.
const ballotPublicInputs = 3

// parseHexBig parses a big-endian hex string (with or without 0x) into a
// big.Int.
func parseHexBig(s string) (*big.Int, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if s == "" {
		return nil, fmt.Errorf("empty hex value")
	}
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return nil, fmt.Errorf("invalid hex value %q", s)
	}
	return v, nil
}

// encKeyFromRTE rebuilds the ElGamal public key from its canonical RTE
// coordinates (big-endian hex).
func encKeyFromRTE(encX, encY string) (*bjjgnark.BJJ, error) {
	rx, err := parseHexBig(encX)
	if err != nil {
		return nil, fmt.Errorf("encX: %w", err)
	}
	ry, err := parseHexBig(encY)
	if err != nil {
		return nil, fmt.Errorf("encY: %w", err)
	}
	// SetPoint returns a fresh point rather than mutating the receiver, so the
	// returned value is the one carrying the coordinates.
	pt := bjjgnark.New().SetPoint(rx, ry)
	key, ok := pt.(*bjjgnark.BJJ)
	if !ok {
		return nil, fmt.Errorf("unexpected curve point type %T", pt)
	}
	return key, nil
}

// chainConfigFromElection parses a persisted ElectionConfig into the
// chain.Config the State engine needs.
func chainConfigFromElection(cfg types.ElectionConfig) (chain.Config, error) {
	pid, err := parseHexBig(cfg.ProcessID)
	if err != nil {
		return chain.Config{}, fmt.Errorf("processID: %w", err)
	}
	bm, err := parseHexBig(cfg.BallotMode)
	if err != nil {
		return chain.Config{}, fmt.Errorf("ballotMode: %w", err)
	}
	cr, err := parseHexBig(cfg.CensusRoot)
	if err != nil {
		return chain.Config{}, fmt.Errorf("censusRoot: %w", err)
	}
	key, err := encKeyFromRTE(cfg.EncX, cfg.EncY)
	if err != nil {
		return chain.Config{}, err
	}
	vkHash, err := davinci.BallotVKLeaf(cfg.VK)
	if err != nil {
		return chain.Config{}, fmt.Errorf("ballot VK hash: %w", err)
	}
	return chain.Config{
		ProcessID:    pid,
		BallotMode:   bm,
		EncKey:       key,
		CensusOrigin: cfg.CensusOrigin,
		CensusRoot:   cr,
		BallotVKHash: vkHash,
	}, nil
}

// voteRules is what ingest checks a vote against, parsed once per election.
type voteRules struct {
	cfg        chain.Config
	processID  vtypes.ProcessID
	ballotMode spec.BallotMode
	ballotVK   *groth16.VerifyingKey
}

// newVoteRules parses an election's configuration into the rules ingest
// applies, rejecting a configuration the batch guest could never accept.
// cfg.VK must be set.
func newVoteRules(ec types.ElectionConfig) (*voteRules, error) {
	if err := checkCensusOrigin(ec.CensusOrigin); err != nil {
		return nil, err
	}
	cfg, err := chainConfigFromElection(ec)
	if err != nil {
		return nil, err
	}
	if !iden3utils.CheckBigIntInField(cfg.CensusRoot) {
		return nil, fmt.Errorf("censusRoot is not a BN254 field element")
	}
	var pid vtypes.ProcessID
	if cfg.ProcessID.BitLen() > 8*vtypes.ProcessIDLen {
		return nil, fmt.Errorf("processID exceeds %d bytes", vtypes.ProcessIDLen)
	}
	cfg.ProcessID.FillBytes(pid[:])
	bm, err := unpackBallotMode(cfg.BallotMode)
	if err != nil {
		return nil, fmt.Errorf("ballotMode: %w", err)
	}
	vk, _, err := parseBallotVK(ec.VK)
	if err != nil {
		return nil, fmt.Errorf("vk: %w", err)
	}
	return &voteRules{cfg: cfg, processID: pid, ballotMode: bm, ballotVK: vk}, nil
}

// unpackBallotMode splits a packed ballot mode into its fields, the inverse
// of spec.BallotMode.Pack. It fails unless the fields pack back to exactly
// packed, and requires 1 to NumFields active fields, as the batch guest does.
func unpackBallotMode(packed *big.Int) (spec.BallotMode, error) {
	field := func(shift, width uint) uint64 {
		mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), width), big.NewInt(1))
		return new(big.Int).And(new(big.Int).Rsh(packed, shift), mask).Uint64()
	}
	bm := spec.BallotMode{
		NumFields:    uint8(field(0, 8)),
		GroupSize:    uint8(field(8, 8)),
		UniqueValues: field(16, 1) == 1,
		CostExponent: uint8(field(17, 8)),
		MaxValue:     field(25, 48),
		MinValue:     field(73, 48),
		MaxValueSum:  field(121, 63),
		MinValueSum:  field(184, 63),
	}
	repacked, err := bm.Pack()
	if err != nil {
		return bm, err
	}
	if repacked.Cmp(packed) != 0 {
		return bm, fmt.Errorf("bits set outside the ballot mode layout")
	}
	if bm.NumFields < 1 || int(bm.NumFields) > davinci.NumFields {
		return bm, fmt.Errorf("numFields %d outside 1..%d", bm.NumFields, davinci.NumFields)
	}
	return bm, nil
}
