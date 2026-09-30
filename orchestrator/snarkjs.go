package orchestrator

import (
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fp"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	groth16 "github.com/consensys/gnark/backend/groth16/bn254"
)

// snarkjs Groth16 JSON, parsed as strictly as the prover's input builder
// (input-gen SnarkJsProof/SnarkJsVk and parse_g1/parse_g2) reads it: decimal
// coordinates below the base field modulus, affine points marked "1", G2 in
// the snarkjs [[x.c0, x.c1], [y.c0, y.c1], ["1", "0"]] order. Anything else
// is rejected rather than risk the prover reading a different point than the
// one verified here. Accepted values are re-encoded canonically, so the batch
// ships exactly what ingest verified.

// snarkjsProof is a snarkjs Groth16 proof.
type snarkjsProof struct {
	A        []string   `json:"pi_a"`
	B        [][]string `json:"pi_b"`
	C        []string   `json:"pi_c"`
	Protocol string     `json:"protocol"`
	Curve    string     `json:"curve"`
}

// snarkjsVK is a snarkjs Groth16 verification key.
type snarkjsVK struct {
	Protocol string     `json:"protocol"`
	Curve    string     `json:"curve"`
	NPublic  int        `json:"nPublic"`
	Alpha    []string   `json:"vk_alpha_1"`
	Beta     [][]string `json:"vk_beta_2"`
	Gamma    [][]string `json:"vk_gamma_2"`
	Delta    [][]string `json:"vk_delta_2"`
	IC       [][]string `json:"IC"`
}

const (
	snarkjsProtocol = "groth16"
	snarkjsCurve    = "bn128"
)

// parseDecimal parses a non-negative decimal integer (digits only) below max.
func parseDecimal(s string, max *big.Int) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty integer")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("%q is not a decimal integer", s)
		}
	}
	v, _ := new(big.Int).SetString(s, 10)
	if v.Cmp(max) >= 0 {
		return nil, fmt.Errorf("%s is out of range", s)
	}
	return v, nil
}

// parseFp parses a decimal base-field coordinate.
func parseFp(s string) (fp.Element, error) {
	var e fp.Element
	v, err := parseDecimal(s, fp.Modulus())
	if err != nil {
		return e, err
	}
	e.SetBigInt(v)
	return e, nil
}

// parseG1 parses an affine G1 point [x, y, "1"] on the curve, not the
// identity (the batch guest rejects an identity proof or vk point).
func parseG1(v []string) (bn254.G1Affine, error) {
	var p bn254.G1Affine
	if len(v) != 3 || v[2] != "1" {
		return p, fmt.Errorf("G1 point must be [x, y, \"1\"]")
	}
	var err error
	if p.X, err = parseFp(v[0]); err != nil {
		return p, err
	}
	if p.Y, err = parseFp(v[1]); err != nil {
		return p, err
	}
	if p.IsInfinity() || !p.IsOnCurve() {
		return p, fmt.Errorf("G1 point not on the curve")
	}
	return p, nil
}

// parseG2 parses an affine G2 point in snarkjs order, on the twist and in
// the prime-order subgroup, not the identity.
func parseG2(v [][]string) (bn254.G2Affine, error) {
	var p bn254.G2Affine
	if len(v) != 3 || len(v[0]) != 2 || len(v[1]) != 2 || len(v[2]) != 2 || v[2][0] != "1" || v[2][1] != "0" {
		return p, fmt.Errorf("G2 point must be [[x0, x1], [y0, y1], [\"1\", \"0\"]]")
	}
	coords := []*fp.Element{&p.X.A0, &p.X.A1, &p.Y.A0, &p.Y.A1}
	for i, s := range []string{v[0][0], v[0][1], v[1][0], v[1][1]} {
		e, err := parseFp(s)
		if err != nil {
			return p, err
		}
		*coords[i] = e
	}
	if p.IsInfinity() || !p.IsOnCurve() || !p.IsInSubGroup() {
		return p, fmt.Errorf("G2 point not in the subgroup")
	}
	return p, nil
}

// fpString renders a coordinate in decimal.
func fpString(e *fp.Element) string { return e.BigInt(new(big.Int)).String() }

func g1JSON(p *bn254.G1Affine) []string { return []string{fpString(&p.X), fpString(&p.Y), "1"} }

func g2JSON(p *bn254.G2Affine) [][]string {
	return [][]string{
		{fpString(&p.X.A0), fpString(&p.X.A1)},
		{fpString(&p.Y.A0), fpString(&p.Y.A1)},
		{"1", "0"},
	}
}

// parseBallotProof parses a snarkjs Groth16 proof and returns it with its
// canonical encoding.
func parseBallotProof(raw []byte) (*groth16.Proof, json.RawMessage, error) {
	var sp snarkjsProof
	if err := json.Unmarshal(raw, &sp); err != nil {
		return nil, nil, fmt.Errorf("decode: %w", err)
	}
	var (
		proof groth16.Proof
		err   error
	)
	if proof.Ar, err = parseG1(sp.A); err != nil {
		return nil, nil, fmt.Errorf("pi_a: %w", err)
	}
	if proof.Bs, err = parseG2(sp.B); err != nil {
		return nil, nil, fmt.Errorf("pi_b: %w", err)
	}
	if proof.Krs, err = parseG1(sp.C); err != nil {
		return nil, nil, fmt.Errorf("pi_c: %w", err)
	}
	canonical, err := json.Marshal(&snarkjsProof{
		A:        g1JSON(&proof.Ar),
		B:        g2JSON(&proof.Bs),
		C:        g1JSON(&proof.Krs),
		Protocol: snarkjsProtocol,
		Curve:    snarkjsCurve,
	})
	if err != nil {
		return nil, nil, err
	}
	return &proof, canonical, nil
}

// parseBallotVK parses a snarkjs Groth16 verification key for the ballot
// proof (ballotPublicInputs public signals) and returns it, precomputed for
// verification, with its canonical encoding.
func parseBallotVK(raw []byte) (*groth16.VerifyingKey, json.RawMessage, error) {
	var sv snarkjsVK
	if err := json.Unmarshal(raw, &sv); err != nil {
		return nil, nil, fmt.Errorf("decode: %w", err)
	}
	if len(sv.IC) != ballotPublicInputs+1 {
		return nil, nil, fmt.Errorf("want %d public inputs, got %d", ballotPublicInputs, len(sv.IC)-1)
	}
	var (
		vk  groth16.VerifyingKey
		err error
	)
	if vk.G1.Alpha, err = parseG1(sv.Alpha); err != nil {
		return nil, nil, fmt.Errorf("vk_alpha_1: %w", err)
	}
	if vk.G2.Beta, err = parseG2(sv.Beta); err != nil {
		return nil, nil, fmt.Errorf("vk_beta_2: %w", err)
	}
	if vk.G2.Gamma, err = parseG2(sv.Gamma); err != nil {
		return nil, nil, fmt.Errorf("vk_gamma_2: %w", err)
	}
	if vk.G2.Delta, err = parseG2(sv.Delta); err != nil {
		return nil, nil, fmt.Errorf("vk_delta_2: %w", err)
	}
	vk.G1.K = make([]bn254.G1Affine, len(sv.IC))
	ic := make([][]string, len(sv.IC))
	for i := range sv.IC {
		if vk.G1.K[i], err = parseG1(sv.IC[i]); err != nil {
			return nil, nil, fmt.Errorf("IC[%d]: %w", i, err)
		}
		ic[i] = g1JSON(&vk.G1.K[i])
	}
	if err := vk.Precompute(); err != nil {
		return nil, nil, err
	}
	canonical, err := json.Marshal(&snarkjsVK{
		Protocol: snarkjsProtocol,
		Curve:    snarkjsCurve,
		NPublic:  ballotPublicInputs,
		Alpha:    g1JSON(&vk.G1.Alpha),
		Beta:     g2JSON(&vk.G2.Beta),
		Gamma:    g2JSON(&vk.G2.Gamma),
		Delta:    g2JSON(&vk.G2.Delta),
		IC:       ic,
	})
	if err != nil {
		return nil, nil, err
	}
	return &vk, canonical, nil
}

// parsePublicInputs parses the ballot proof's public signals as decimal
// scalar-field elements.
func parsePublicInputs(pubs []string) ([]*big.Int, fr.Vector, error) {
	if len(pubs) != ballotPublicInputs {
		return nil, nil, fmt.Errorf("want %d signals, got %d", ballotPublicInputs, len(pubs))
	}
	vals := make([]*big.Int, len(pubs))
	vec := make(fr.Vector, len(pubs))
	for i, s := range pubs {
		v, err := parseDecimal(s, fr.Modulus())
		if err != nil {
			return nil, nil, fmt.Errorf("[%d]: %w", i, err)
		}
		vals[i] = v
		vec[i].SetBigInt(v)
	}
	return vals, vec, nil
}
