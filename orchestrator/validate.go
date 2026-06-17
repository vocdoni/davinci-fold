package orchestrator

import (
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/vocdoni/davinci-node/circuits/ballotproof"
	"github.com/vocdoni/davinci-fold/crypto"
	bjjgnark "github.com/vocdoni/davinci-node/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-node/crypto/elgamal"
	"github.com/vocdoni/davinci-node/crypto/signatures/ethereum"
	"github.com/vocdoni/davinci-node/util/circomgnark"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/chain"
)

// VoteSubmission is the decoded, self-authenticating vote payload a voter
// sends to POST /elections/{id}/votes. It carries the light key parts the
// orchestrator applies to state plus the heavy ballot-proof material the
// worker circuit re-verifies in-batch.
type VoteSubmission struct {
	// VoteID is the unique vote identifier (API-level bytes).
	VoteID []byte `json:"vote_id"`
	// Address is the voter Ethereum address (20 bytes).
	Address []byte `json:"address"`
	// CensusIdx is the voter's census index.
	CensusIdx int `json:"census_idx"`
	// AddressLo16 is the low 16 bits of the address (ballot state-tree key).
	AddressLo16 uint64 `json:"address_lo16"`
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
	// Sig is the voter ECDSA signature over the ballot.
	Sig json.RawMessage `json:"sig"`
	// Census is the lean-IMT membership proof for the voter.
	Census davinci.CensusProof `json:"census"`
}

// voteProofBundle is the per-vote proving material persisted in Vote.Payload
// and re-assembled into the batch ProveRequest at seal time.
type voteProofBundle struct {
	Proof        json.RawMessage     `cbor:"proof"`
	PublicInputs []string            `cbor:"publicInputs"`
	Sig          json.RawMessage     `cbor:"sig"`
	Census       davinci.CensusProof `cbor:"census"`
}

// Validator decides whether a submission may enter an election's vote log.
// Keeping it an interface lets the structural validator be swapped for the full
// cryptographic verifier without touching the ingest path.
type Validator interface {
	Validate(cfg chain.Config, sub *VoteSubmission) error
}

// structuralValidator performs only cheap well-formedness and binding checks.
// It does NOT verify the Groth16 ballot proof or the ECDSA signature, so it is
// not safe for production ingest; it exists for unit tests that exercise the
// ingest/lifecycle mechanics with synthetic ballots. Production wiring uses
// cryptoValidator (the nil-Options default).
type structuralValidator struct{}

func (structuralValidator) Validate(cfg chain.Config, sub *VoteSubmission) error {
	if len(sub.VoteID) == 0 {
		return fmt.Errorf("missing vote_id")
	}
	if len(sub.Ballot) == 0 {
		return fmt.Errorf("missing ballot")
	}
	if err := elgamal.NewBallot(bjjgnark.New()).Deserialize(sub.Ballot); err != nil {
		return fmt.Errorf("malformed ballot: %w", err)
	}
	if len(sub.Proof) == 0 {
		return fmt.Errorf("missing ballot proof")
	}
	if len(sub.PublicInputs) == 0 {
		return fmt.Errorf("missing public_inputs")
	}
	if len(sub.Sig) == 0 {
		return fmt.Errorf("missing signature")
	}
	root, err := parseHexBig(sub.Census.Root)
	if err != nil {
		return fmt.Errorf("census root: %w", err)
	}
	if root.Cmp(cfg.CensusRoot) != 0 {
		return fmt.Errorf("census root mismatch")
	}
	return nil
}

// voteSig is the on-disk ECDSA signature format input-gen and the integration
// ballot generator emit (sigJSON). Only R, S and the recovery bit are needed to
// recover the signer; the embedded public key and address are client-asserted
// and deliberately ignored (the address is recovered from the signature).
type voteSig struct {
	SignatureR string `json:"signature_r"`
	SignatureS string `json:"signature_s"`
	SignatureV byte   `json:"signature_v"`
}

// cryptoValidator fully authenticates a self-submitted vote before it enters an
// election's log: it checks well-formedness and the light/heavy key bindings,
// verifies the voter's ECDSA signature over the vote ID, and verifies the
// Groth16 ballot proof against the protocol ballot circuit's verification key.
//
// This mirrors davinci-node's newVote handler. The one check it does not
// replicate is the host-side recomputation of the ballot inputs hash
// (BallotInputsHashIden3), because that needs the unpacked spec.BallotMode and
// the voter weight, neither of which crosses davinci-fold's abstracted ingest
// boundary. That ballot<->inputs-hash tie is instead enforced in-circuit by the
// batch guest, which recomputes the hash from the submitted ballot and
// re-verifies the proof against it; a ballot whose content does not match its
// proof can therefore only fail its own batch STARK, never corrupt the
// verifiable tally (the final PLONK + digest attest every transition).
type cryptoValidator struct {
	// ballotVK is the raw snarkjs verification key JSON of the ballot proof
	// circuit. It is the protocol-fixed artifact, the same key the batch guest
	// verifies each ballot proof against.
	ballotVK []byte
}

// newCryptoValidator builds the production validator bound to the protocol
// ballot-proof verification key.
func newCryptoValidator() *cryptoValidator {
	return &cryptoValidator{ballotVK: ballotproof.CircomVerificationKey}
}

func (v *cryptoValidator) Validate(cfg chain.Config, sub *VoteSubmission) error {
	// Well-formedness.
	if len(sub.Address) != common.AddressLength {
		return fmt.Errorf("address must be %d bytes", common.AddressLength)
	}
	if len(sub.VoteID) == 0 {
		return fmt.Errorf("missing vote_id")
	}
	if len(sub.Ballot) == 0 {
		return fmt.Errorf("missing ballot")
	}
	if err := elgamal.NewBallot(bjjgnark.New()).Deserialize(sub.Ballot); err != nil {
		return fmt.Errorf("malformed ballot: %w", err)
	}
	if len(sub.Proof) == 0 {
		return fmt.Errorf("missing ballot proof")
	}
	if len(sub.Sig) == 0 {
		return fmt.Errorf("missing signature")
	}
	if len(sub.PublicInputs) != 3 {
		return fmt.Errorf("public_inputs: want 3 signals, got %d", len(sub.PublicInputs))
	}

	// Census root binding: the proof must target this election's census.
	root, err := parseHexBig(sub.Census.Root)
	if err != nil {
		return fmt.Errorf("census root: %w", err)
	}
	if root.Cmp(cfg.CensusRoot) != 0 {
		return fmt.Errorf("census root mismatch")
	}

	// Bind the public signals and the light state-tree keys to the voter's
	// address and vote ID, so the proof cannot attest one identity while the
	// applied state mutation carries another.
	addr := new(big.Int).SetBytes(sub.Address)
	voteID := new(big.Int).SetBytes(sub.VoteID)
	pubAddr, ok := new(big.Int).SetString(sub.PublicInputs[0], 10)
	if !ok {
		return fmt.Errorf("public_inputs[0]: not a decimal integer")
	}
	pubVoteID, ok := new(big.Int).SetString(sub.PublicInputs[1], 10)
	if !ok {
		return fmt.Errorf("public_inputs[1]: not a decimal integer")
	}
	if pubAddr.Cmp(addr) != 0 {
		return fmt.Errorf("public_inputs address does not match submission address")
	}
	if pubVoteID.Cmp(voteID) != 0 {
		return fmt.Errorf("public_inputs vote ID does not match submission vote ID")
	}
	if !voteID.IsUint64() || voteID.Uint64() != sub.VoteIDKey {
		return fmt.Errorf("vote_id_key does not match vote_id")
	}
	if addr.Uint64()&0xFFFF != sub.AddressLo16 {
		return fmt.Errorf("address_lo16 does not match address")
	}

	// ECDSA signature: recover the signer from (R,S,V) over the padded vote ID
	// and require it to match the submitted address. SetBytes rejects high-S
	// (malleability), so a re-signed duplicate cannot slip past dedup.
	sig, err := parseVoteSig(sub.Sig)
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	if sigOk, _ := sig.Verify(crypto.PadToSign(sub.VoteID), common.BytesToAddress(sub.Address)); !sigOk {
		return fmt.Errorf("signature verification failed")
	}

	// Groth16 ballot proof: verify the snarkjs proof against the protocol
	// ballot-circuit VK and the submitted public signals. A bogus proof is
	// rejected here rather than poisoning the batch STARK at prove time.
	if err := v.verifyBallotProof(sub); err != nil {
		return fmt.Errorf("ballot proof: %w", err)
	}
	return nil
}

// verifyBallotProof checks the snarkjs Groth16 ballot proof against the ballot
// circuit verification key and the submission's public signals.
func (v *cryptoValidator) verifyBallotProof(sub *VoteSubmission) error {
	vk, err := circomgnark.UnmarshalCircomVerificationKeyJSON(v.ballotVK)
	if err != nil {
		return fmt.Errorf("load verification key: %w", err)
	}
	proof, err := circomgnark.UnmarshalCircomProofJSON(sub.Proof)
	if err != nil {
		return fmt.Errorf("decode proof: %w", err)
	}
	gnarkProof, err := circomgnark.ConvertCircomToGnark(vk, proof, sub.PublicInputs)
	if err != nil {
		return fmt.Errorf("convert proof: %w", err)
	}
	if ok, err := gnarkProof.Verify(); err != nil || !ok {
		return fmt.Errorf("invalid proof")
	}
	return nil
}

// parseVoteSig decodes the sigJSON envelope into an ECDSASignature. It returns
// an error on malformed hex or a high-S (rejected by SetBytes) signature.
func parseVoteSig(raw json.RawMessage) (*ethereum.ECDSASignature, error) {
	var js voteSig
	if err := json.Unmarshal(raw, &js); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	r, err := parseHexBig(js.SignatureR)
	if err != nil {
		return nil, fmt.Errorf("R: %w", err)
	}
	s, err := parseHexBig(js.SignatureS)
	if err != nil {
		return nil, fmt.Errorf("S: %w", err)
	}
	buf := make([]byte, 65)
	r.FillBytes(buf[0:32])
	s.FillBytes(buf[32:64])
	buf[64] = js.SignatureV
	sig := new(ethereum.ECDSASignature).SetBytes(buf)
	if sig == nil {
		return nil, fmt.Errorf("invalid signature encoding")
	}
	return sig, nil
}
