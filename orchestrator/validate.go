package orchestrator

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	"github.com/consensys/gnark-crypto/ecc/bn254/twistededwards"
	groth16 "github.com/consensys/gnark/backend/groth16/bn254"
	"github.com/ethereum/go-ethereum/common"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/vocdoni/davinci-fold/crypto"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/circuits/ballotproof"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/ecc"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/elgamal"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/signatures/ethereum"
	vtypes "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/types"
)

// VoteSubmission is the decoded, self-authenticating vote payload a voter
// sends to POST /elections/{id}/votes. It carries the light key parts the
// orchestrator applies to state plus the heavy ballot-proof material the
// worker circuit re-verifies in-batch.
type VoteSubmission struct {
	// VoteID is the unique vote identifier: VoteIDKey as 8 big-endian bytes.
	VoteID []byte `json:"vote_id"`
	// Address is the voter Ethereum address (20 bytes).
	Address []byte `json:"address"`
	// VoteIDKey is the numeric vote-ID state-tree key (bit 63 set).
	VoteIDKey uint64 `json:"vote_id_key"`
	// Ballot is the voter-encrypted ElGamal ballot, serialized via
	// elgamal.Ballot.Serialize.
	Ballot []byte `json:"ballot"`
	// Proof is the snarkjs Groth16 ballot proof object.
	Proof json.RawMessage `json:"proof"`
	// PublicInputs are the public signals for the ballot proof:
	// [address_dec, voteID_dec, inputsHash_dec].
	PublicInputs []string `json:"public_inputs"`
	// Sig is the voter's ECDSA signature over the vote ID.
	Sig json.RawMessage `json:"sig"`
	// Census is the lean-IMT membership proof for the voter. Its leaf binds
	// the voter's address, which fixes the ballot slot (CensusProof.SlotKey).
	Census davinci.CensusProof `json:"census"`
}

// voteProofBundle is the per-vote proving material persisted in Vote.Payload
// and re-assembled into the batch ProveRequest at seal time, in the encoding
// the prover parses.
type voteProofBundle struct {
	Proof        json.RawMessage     `cbor:"proof"`
	PublicInputs []string            `cbor:"publicInputs"`
	Sig          json.RawMessage     `cbor:"sig"`
	Census       davinci.CensusProof `cbor:"census"`
}

// Validator decides whether a submission may enter an election's vote log
// and returns its proving material as the batch will carry it. A vote it
// accepts must never make a batch fail in the guest: one failed batch blocks
// the whole election. Production uses cryptoValidator; unit tests use
// structuralValidator.
type Validator interface {
	Validate(rules *voteRules, sub *VoteSubmission) (*voteProofBundle, error)
}

// checkVote runs the checks that need neither the ballot proof nor the
// signature: the vote ID, the ballot and the census proof. It returns the
// decoded ballot, the weight the census leaf carries and the canonical census
// proof.
func checkVote(rules *voteRules, sub *VoteSubmission) (*elgamal.Ballot, *big.Int, davinci.CensusProof, error) {
	if len(sub.Address) != common.AddressLength {
		return nil, nil, davinci.CensusProof{}, fmt.Errorf("%w: address must be %d bytes", ErrMalformedVote, common.AddressLength)
	}
	// One encoding per vote-ID key, so the duplicate check on the stored ID is
	// the state tree's: a key inserted twice fails the batch.
	if len(sub.VoteID) != 8 {
		return nil, nil, davinci.CensusProof{}, fmt.Errorf("%w: vote_id must be 8 bytes, got %d", ErrMalformedVote, len(sub.VoteID))
	}
	if binary.BigEndian.Uint64(sub.VoteID) != sub.VoteIDKey {
		return nil, nil, davinci.CensusProof{}, fmt.Errorf("%w: vote_id_key does not match vote_id", ErrMalformedVote)
	}
	if !vtypes.VoteID(sub.VoteIDKey).Valid() {
		return nil, nil, davinci.CensusProof{}, fmt.Errorf("%w: vote ID %#x outside the vote-ID namespace", ErrMalformedVote, sub.VoteIDKey)
	}
	ballot, err := checkBallot(rules, sub.Ballot)
	if err != nil {
		return nil, nil, davinci.CensusProof{}, fmt.Errorf("%w: %w", ErrMalformedVote, err)
	}
	weight, census, err := verifyCensusProof(rules.cfg.CensusRoot, sub.Address, sub.Census)
	if err != nil {
		return nil, nil, davinci.CensusProof{}, fmt.Errorf("%w: %w", ErrInvalidCensusProof, err)
	}
	return ballot, weight, census, nil
}

// checkBallot decodes the ballot and checks what the batch guest assumes of
// the ciphertexts it re-encrypts and accumulates: every field past the ballot
// mode's numFields is the identity (the guest rejects anything else), and
// every other point is on BabyJubJub (a precondition of the guest's point
// addition). A sound ballot proof implies the second; the first it leaves
// unconstrained, so the inputs hash does not cover it.
func checkBallot(rules *voteRules, raw []byte) (*elgamal.Ballot, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("missing ballot")
	}
	ballot := elgamal.NewBallot(rules.cfg.EncKey)
	if err := ballot.Deserialize(raw); err != nil {
		return nil, fmt.Errorf("malformed ballot: %w", err)
	}
	nf := int(rules.ballotMode.NumFields)
	for i, ct := range ballot.Ciphertexts {
		for _, p := range []ecc.Point{ct.C1, ct.C2} {
			x, y := p.Point()
			if i >= nf {
				if x.Sign() != 0 || y.Cmp(big.NewInt(1)) != 0 {
					return nil, fmt.Errorf("malformed ballot: field %d must be the identity, the election has %d fields", i, nf)
				}
				continue
			}
			var pt twistededwards.PointAffine
			pt.X.SetBigInt(x)
			pt.Y.SetBigInt(y)
			if !pt.IsOnCurve() {
				return nil, fmt.Errorf("malformed ballot: field %d is not on BabyJubJub", i)
			}
		}
	}
	return ballot, nil
}

// structuralValidator performs only the checks of checkVote. It does NOT
// verify the Groth16 ballot proof, its public inputs or the ECDSA signature,
// so it is not safe for production ingest; it exists for unit tests that
// exercise the ingest/lifecycle mechanics with synthetic ballots. Production
// wiring uses cryptoValidator (the nil-Options default).
type structuralValidator struct{}

func (structuralValidator) Validate(rules *voteRules, sub *VoteSubmission) (*voteProofBundle, error) {
	if len(sub.Proof) == 0 {
		return nil, fmt.Errorf("%w: missing proof", ErrInvalidBallotProof)
	}
	if len(sub.PublicInputs) == 0 {
		return nil, fmt.Errorf("%w: missing public_inputs", ErrInvalidBallotProof)
	}
	if len(sub.Sig) == 0 {
		return nil, fmt.Errorf("%w: missing signature", ErrInvalidSignature)
	}
	_, _, census, err := checkVote(rules, sub)
	if err != nil {
		return nil, err
	}
	return &voteProofBundle{Proof: sub.Proof, PublicInputs: sub.PublicInputs, Sig: sub.Sig, Census: census}, nil
}

// voteSig is the signature a voter submits (the sigJSON the integration ballot
// generator emits). Only R, S and the recovery bit are read; the signer is
// recovered from them.
type voteSig struct {
	SignatureR string `json:"signature_r"`
	SignatureS string `json:"signature_s"`
	SignatureV byte   `json:"signature_v"`
}

// proverSig is the signature as the prover's input builder reads it
// (input-gen EcdsaSig, every field but signature_v required). The guest only
// uses r, s and the recovery bit; the rest is informational.
type proverSig struct {
	PublicKeyX string `json:"public_key_x"`
	PublicKeyY string `json:"public_key_y"`
	SignatureR string `json:"signature_r"`
	SignatureS string `json:"signature_s"`
	SignatureV byte   `json:"signature_v"`
	VoteID     uint64 `json:"vote_id"`
	Address    string `json:"address"`
}

// cryptoValidator fully authenticates a self-submitted vote and checks every
// guest rule that depends on what the voter sends, so an accepted vote cannot
// fail its batch: checkVote, the ballot proof's public inputs (bound to the
// address, the vote ID and, through the inputs hash, to the election, the
// ballot and the census weight), the ECDSA signature over the vote ID and the
// Groth16 ballot proof against the election's verification key. The proof,
// public inputs and signature are stored re-encoded canonically, so the prover
// parses exactly the values verified here.
type cryptoValidator struct{}

func (cryptoValidator) Validate(rules *voteRules, sub *VoteSubmission) (*voteProofBundle, error) {
	if len(sub.Proof) == 0 {
		return nil, fmt.Errorf("%w: missing proof", ErrInvalidBallotProof)
	}
	if len(sub.Sig) == 0 {
		return nil, fmt.Errorf("%w: missing signature", ErrInvalidSignature)
	}
	ballot, weight, census, err := checkVote(rules, sub)
	if err != nil {
		return nil, err
	}

	// Bind the public signals to the voter's address and vote ID, so the proof
	// cannot attest one identity while the applied state mutation carries
	// another.
	pubs, pubVec, err := parsePublicInputs(sub.PublicInputs)
	if err != nil {
		return nil, fmt.Errorf("%w: public_inputs: %w", ErrInvalidBallotProof, err)
	}
	address := new(big.Int).SetBytes(sub.Address)
	if pubs[0].Cmp(address) != 0 {
		return nil, fmt.Errorf("%w: public_inputs address does not match submission address", ErrInvalidBallotProof)
	}
	if !pubs[1].IsUint64() || pubs[1].Uint64() != sub.VoteIDKey {
		return nil, fmt.Errorf("%w: public_inputs vote ID does not match submission vote ID", ErrInvalidBallotProof)
	}
	// The inputs hash commits the proof to the election (process ID, ballot
	// mode, encryption key), the ballot as submitted and the census weight.
	// The batch guest recomputes it from the same values.
	inputsHash, err := ballotproof.BallotInputsHashGnark(rules.processID, rules.ballotMode, rules.cfg.EncKey,
		vtypes.HexBytes(sub.Address), vtypes.VoteID(sub.VoteIDKey), ballot, (*vtypes.BigInt)(weight))
	if err != nil {
		return nil, fmt.Errorf("%w: inputs hash: %w", ErrInvalidBallotProof, err)
	}
	if pubs[2].Cmp(inputsHash.MathBigInt()) != 0 {
		return nil, fmt.Errorf("%w: public_inputs inputs hash does not match the ballot, election and census weight",
			ErrInvalidBallotProof)
	}

	// ECDSA signature: recover the signer from (R,S,V) over the padded vote ID
	// and require it to match the submitted address.
	sig, err := parseVoteSig(sub.Sig)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSignature, err)
	}
	sigOk, pubKey := sig.Verify(crypto.PadToSign(sub.VoteID), common.BytesToAddress(sub.Address))
	if !sigOk {
		return nil, fmt.Errorf("%w: it does not recover to the voter's address", ErrInvalidSignature)
	}

	// Groth16 ballot proof against the election's ballot VK (the one the
	// batch ships to the provers). A bogus proof is rejected here rather than
	// poisoning the batch STARK at prove time.
	proof, proofJSON, err := parseBallotProof(sub.Proof)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidBallotProof, err)
	}
	if err := groth16.Verify(proof, rules.ballotVK, pubVec); err != nil {
		return nil, fmt.Errorf("%w: it does not verify under the election's vk", ErrInvalidBallotProof)
	}

	rsv := sig.Bytes()
	sigJSON, err := json.Marshal(&proverSig{
		PublicKeyX: "0x" + hex.EncodeToString(pubKey[1:33]),
		PublicKeyY: "0x" + hex.EncodeToString(pubKey[33:65]),
		SignatureR: "0x" + hex.EncodeToString(rsv[:32]),
		SignatureS: "0x" + hex.EncodeToString(rsv[32:64]),
		SignatureV: rsv[64],
		VoteID:     sub.VoteIDKey,
		Address:    address.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("encode signature: %w", err)
	}
	return &voteProofBundle{
		Proof:        proofJSON,
		PublicInputs: []string{address.String(), strconv.FormatUint(sub.VoteIDKey, 10), pubs[2].String()},
		Sig:          sigJSON,
		Census:       census,
	}, nil
}

// parseVoteSig decodes a voter signature into an ECDSASignature the guest can
// recover a key from: r and s in [1, n-1] (s at most n/2, which also stops a
// re-signed duplicate) and a recovery bit of 0 or 1 (27 or 28 are accepted and
// normalised).
func parseVoteSig(raw json.RawMessage) (*ethereum.ECDSASignature, error) {
	var js voteSig
	if err := json.Unmarshal(raw, &js); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	n := ethcrypto.S256().Params().N
	r, err := parseHexBig(js.SignatureR)
	if err != nil {
		return nil, fmt.Errorf("r: %w", err)
	}
	s, err := parseHexBig(js.SignatureS)
	if err != nil {
		return nil, fmt.Errorf("s: %w", err)
	}
	if r.Sign() == 0 || r.Cmp(n) >= 0 || s.Sign() == 0 || s.Cmp(n) >= 0 {
		return nil, fmt.Errorf("r and s must be in [1, n-1]")
	}
	buf := make([]byte, 65)
	r.FillBytes(buf[0:32])
	s.FillBytes(buf[32:64])
	buf[64] = js.SignatureV
	sig := new(ethereum.ECDSASignature).SetBytes(buf)
	if sig == nil {
		return nil, fmt.Errorf("invalid signature encoding")
	}
	if v := sig.Bytes()[64]; v > 1 {
		return nil, fmt.Errorf("recovery id %d, want 0 or 1", v)
	}
	return sig, nil
}
