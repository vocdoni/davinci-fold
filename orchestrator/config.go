package orchestrator

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/vocdoni/davinci-fold/types"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
	bjjgnark "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/ecc/bjj_gnark"
)

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
