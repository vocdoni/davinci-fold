package orchestrator

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/consensys/gnark-crypto/ecc/bn254/fp"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	qt "github.com/frankban/quicktest"
	"github.com/vocdoni/davinci-fold/types"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/circuits/ballotproof"
	bjjgnark "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/ecc/bjj_gnark"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/crypto/elgamal"
	"github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/spec"
	spectestutil "github.com/vocdoni/davinci-zkvm/go-sdk/vocdoni/spec/testutil"
)

func assertErr(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		qt.Assert(t, err, qt.IsNil)
		return
	}
	qt.Assert(t, err, qt.ErrorMatches, ".*"+regexp.QuoteMeta(want)+".*")
}

// TestIngestVoteShape drives the vote ID and ballot checks through SubmitVote.
func TestIngestVoteShape(t *testing.T) {
	e, _ := newTestEngine(t)
	defer e.Stop()
	el, encKey := testElection(t, 0x30, time.Now().Add(time.Hour))
	qt.Assert(t, e.CreateElection("admin", el), qt.IsNil)
	nf := int(spectestutil.FixedBallotMode().NumFields)

	tests := []struct {
		name    string
		mutate  func(sub *VoteSubmission)
		wantErr string
	}{
		{
			name:    "vote ID with a leading zero byte",
			mutate:  func(sub *VoteSubmission) { sub.VoteID = append([]byte{0}, sub.VoteID...) },
			wantErr: "vote_id must be 8 bytes, got 9",
		},
		{
			name:    "vote ID key differs",
			mutate:  func(sub *VoteSubmission) { sub.VoteIDKey++ },
			wantErr: "vote_id_key does not match vote_id",
		},
		{
			name: "vote ID outside the namespace",
			mutate: func(sub *VoteSubmission) {
				sub.VoteIDKey &^= davinci.VoteIDMin
				sub.VoteID[0] &^= 0x80
			},
			wantErr: "outside the vote-ID namespace",
		},
		{
			name: "padded field not the identity",
			mutate: func(sub *VoteSubmission) {
				b := elgamal.NewBallot(encKey)
				qt.Assert(t, b.Deserialize(sub.Ballot), qt.IsNil)
				b.Ciphertexts[nf] = b.Ciphertexts[0]
				sub.Ballot = b.Serialize()
			},
			wantErr: "field 6 must be the identity",
		},
		{
			name: "active field off the curve",
			mutate: func(sub *VoteSubmission) {
				b := elgamal.NewBallot(encKey)
				qt.Assert(t, b.Deserialize(sub.Ballot), qt.IsNil)
				x, y := b.Ciphertexts[1].C2.Point()
				b.Ciphertexts[1].C2 = bjjgnark.New().SetPoint(x, y.Add(y, big.NewInt(1)))
				sub.Ballot = b.Serialize()
			},
			wantErr: "field 1 is not on BabyJubJub",
		},
		{
			name:    "truncated ballot",
			mutate:  func(sub *VoteSubmission) { sub.Ballot = sub.Ballot[:100] },
			wantErr: "malformed ballot",
		},
		{name: "valid"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub := makeVote(t, encKey, 0, byte(0x40+i))
			if tc.mutate != nil {
				tc.mutate(sub)
			}
			_, err := e.SubmitVote(el.ID, sub)
			assertErr(t, err, tc.wantErr)
		})
	}
}

func TestUnpackBallotMode(t *testing.T) {
	bm := spectestutil.FixedBallotMode()
	packed, err := bm.Pack()
	qt.Assert(t, err, qt.IsNil)
	got, err := unpackBallotMode(packed)
	qt.Assert(t, err, qt.IsNil)
	qt.Assert(t, got, qt.DeepEquals, bm)

	full := spec.BallotMode{
		NumFields: 16, GroupSize: 16, UniqueValues: true, CostExponent: 255,
		MaxValue: 1<<48 - 1, MinValue: 1<<48 - 2, MaxValueSum: 1<<63 - 1, MinValueSum: 1<<63 - 2,
	}
	packed, err = full.Pack()
	qt.Assert(t, err, qt.IsNil)
	got, err = unpackBallotMode(packed)
	qt.Assert(t, err, qt.IsNil)
	qt.Assert(t, got, qt.DeepEquals, full)

	for name, tc := range map[string]struct {
		packed  *big.Int
		wantErr string
	}{
		"bit above the layout": {new(big.Int).Or(big.NewInt(6), new(big.Int).Lsh(big.NewInt(1), 247)), "outside the ballot mode layout"},
		"no fields":            {big.NewInt(0), "numFields 0 outside 1..16"},
		"17 fields":            {big.NewInt(17), "numFields 17 outside 1..16"},
		"group above fields":   {big.NewInt(3 | 4<<8), "groupSize exceeds numFields"},
	} {
		_, err := unpackBallotMode(tc.packed)
		qt.Assert(t, err, qt.ErrorMatches, ".*"+regexp.QuoteMeta(tc.wantErr)+".*", qt.Commentf("%s", name))
	}
}

// TestCreateElectionConfigChecks checks configurations no batch could prove
// are refused at creation.
func TestCreateElectionConfigChecks(t *testing.T) {
	e, _ := newTestEngine(t)
	defer e.Stop()
	for i, tc := range []struct {
		name    string
		mutate  func(c *types.ElectionConfig)
		wantErr string
	}{
		{name: "processID over 31 bytes", mutate: func(c *types.ElectionConfig) { c.ProcessID = "0x" + strings.Repeat("11", 32) }, wantErr: "processID exceeds 31 bytes"},
		{name: "census root not a field element", mutate: func(c *types.ElectionConfig) { c.CensusRoot = "0x" + strings.Repeat("ff", 32) }, wantErr: "censusRoot is not a BN254 field element"},
		{name: "17 fields", mutate: func(c *types.ElectionConfig) { c.BallotMode = "0x11" }, wantErr: "ballotMode: numFields 17 outside 1..16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			el, _ := testElection(t, byte(0x50+i), time.Now().Add(time.Hour))
			tc.mutate(&el.Config)
			assertErr(t, e.CreateElection("admin", el), tc.wantErr)
		})
	}
}

// vkMap is the davinci-circom vk as a JSON object, for editing.
func vkMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	qt.Assert(t, json.Unmarshal(ballotproof.CircomVerificationKey, &m), qt.IsNil)
	return m
}

func TestParseBallotVK(t *testing.T) {
	// The canonical encoding keeps the key the config commits to.
	_, canonical, err := parseBallotVK(ballotproof.CircomVerificationKey)
	qt.Assert(t, err, qt.IsNil)
	want, err := davinci.BallotVKLeaf(ballotproof.CircomVerificationKey)
	qt.Assert(t, err, qt.IsNil)
	got, err := davinci.BallotVKLeaf(canonical)
	qt.Assert(t, err, qt.IsNil)
	qt.Assert(t, got.Cmp(want), qt.Equals, 0)
	_, again, err := parseBallotVK(canonical)
	qt.Assert(t, err, qt.IsNil)
	qt.Assert(t, again, qt.DeepEquals, canonical)

	for _, tc := range []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"identity point", func(m map[string]any) { m["vk_alpha_1"].([]any)[2] = "0" }, "vk_alpha_1: G1 point must be"},
		{"hex coordinate", func(m map[string]any) { m["IC"].([]any)[1].([]any)[0] = "0x1" }, "IC[1]: \"0x1\" is not a decimal integer"},
		{"coordinate above the modulus", func(m map[string]any) {
			m["IC"].([]any)[0].([]any)[0] = fp.Modulus().String()
		}, "IC[0]: " + fp.Modulus().String() + " is out of range"},
		{"not on the curve", func(m map[string]any) { m["IC"].([]any)[0].([]any)[1] = "5" }, "IC[0]: G1 point not on the curve"},
		{"G2 in another order", func(m map[string]any) {
			b := m["vk_beta_2"].([]any)
			x := b[0].([]any)
			x[0], x[1] = x[1], x[0]
		}, "vk_beta_2: G2 point not in the subgroup"},
		{"G2 short", func(m map[string]any) { m["vk_delta_2"] = m["vk_delta_2"].([]any)[:2] }, "vk_delta_2: G2 point must be"},
		{"two public inputs", func(m map[string]any) { m["IC"] = m["IC"].([]any)[:3] }, "want 3 public inputs, got 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := vkMap(t)
			tc.mutate(m)
			raw, err := json.Marshal(m)
			qt.Assert(t, err, qt.IsNil)
			_, _, err = parseBallotVK(raw)
			assertErr(t, err, tc.wantErr)
		})
	}
}

func TestParseBallotProof(t *testing.T) {
	// A proof made of well-formed points from the vk: it parses (it does not
	// verify) and re-encodes canonically, with protocol and curve added.
	m := vkMap(t)
	proof := map[string]any{"pi_a": m["vk_alpha_1"], "pi_b": m["vk_beta_2"], "pi_c": m["IC"].([]any)[0]}
	raw, err := json.Marshal(proof)
	qt.Assert(t, err, qt.IsNil)
	_, canonical, err := parseBallotProof(raw)
	qt.Assert(t, err, qt.IsNil)
	proof["protocol"], proof["curve"] = "groth16", "bn128"
	qt.Assert(t, []byte(canonical), qt.JSONEquals, proof)

	for _, tc := range []struct {
		name    string
		mutate  func(p map[string]any)
		wantErr string
	}{
		{"projective z", func(p map[string]any) { p["pi_c"] = []any{"1", "2", "2"} }, "pi_c: G1 point must be"},
		{"missing pi_b", func(p map[string]any) { delete(p, "pi_b") }, "pi_b: G2 point must be"},
		{"signed coordinate", func(p map[string]any) {
			a := append([]any(nil), p["pi_a"].([]any)...)
			a[0] = "+" + a[0].(string)
			p["pi_a"] = a
		}, "pi_a: \"+"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := map[string]any{"pi_a": m["vk_alpha_1"], "pi_b": m["vk_beta_2"], "pi_c": m["IC"].([]any)[0]}
			tc.mutate(p)
			raw, err := json.Marshal(p)
			qt.Assert(t, err, qt.IsNil)
			_, _, err = parseBallotProof(raw)
			assertErr(t, err, tc.wantErr)
		})
	}
}

func TestParsePublicInputs(t *testing.T) {
	_, _, err := parsePublicInputs([]string{"1", "2", "3"})
	qt.Assert(t, err, qt.IsNil)
	for _, tc := range []struct {
		pubs    []string
		wantErr string
	}{
		{[]string{"1", "2"}, "want 3 signals, got 2"},
		{[]string{"1", "+2", "3"}, "[1]: \"+2\" is not a decimal integer"},
		{[]string{"1", "2", "0x3"}, "[2]: \"0x3\" is not a decimal integer"},
		{[]string{"", "2", "3"}, "[0]: empty integer"},
		{[]string{"1", "2", fr.Modulus().String()}, "[2]: " + fr.Modulus().String() + " is out of range"},
	} {
		_, _, err := parsePublicInputs(tc.pubs)
		assertErr(t, err, tc.wantErr)
	}
}

func TestParseVoteSig(t *testing.T) {
	n := "fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141"
	r := "0x" + strings.Repeat("11", 32)
	s := "0x" + strings.Repeat("22", 32)
	sigJSON := func(r, s string, v int) json.RawMessage {
		raw, err := json.Marshal(map[string]any{"signature_r": r, "signature_s": s, "signature_v": v})
		qt.Assert(t, err, qt.IsNil)
		return raw
	}
	for _, tc := range []struct {
		name    string
		sig     json.RawMessage
		wantV   byte
		wantErr string
	}{
		{name: "v 1", sig: sigJSON(r, s, 1), wantV: 1},
		{name: "v 28 normalised", sig: sigJSON(r, s, 28), wantV: 1},
		{name: "v 27 normalised", sig: sigJSON(strings.TrimPrefix(strings.ToUpper(r), "0X"), s, 27), wantV: 0},
		{name: "v 2", sig: sigJSON(r, s, 2), wantErr: "recovery id 2, want 0 or 1"},
		{name: "v 5", sig: sigJSON(r, s, 5), wantErr: "invalid signature encoding"},
		{name: "r zero", sig: sigJSON("0x00", s, 0), wantErr: "r and s must be in [1, n-1]"},
		{name: "r = n", sig: sigJSON("0x"+n, s, 0), wantErr: "r and s must be in [1, n-1]"},
		{name: "r over 32 bytes", sig: sigJSON("0x01"+strings.Repeat("00", 32), s, 0), wantErr: "r and s must be in [1, n-1]"},
		{name: "high s", sig: sigJSON(r, "0x"+strings.Repeat("ff", 31)+"00", 0), wantErr: "r and s must be in [1, n-1]"},
		{name: "s above n/2", sig: sigJSON(r, "0x"+strings.Repeat("7f", 1)+strings.Repeat("ff", 31), 0), wantErr: "invalid signature encoding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sig, err := parseVoteSig(tc.sig)
			assertErr(t, err, tc.wantErr)
			if tc.wantErr == "" {
				b := sig.Bytes()
				qt.Assert(t, b[64], qt.Equals, tc.wantV)
				qt.Assert(t, hex.EncodeToString(b[:32]), qt.Equals, strings.Repeat("11", 32))
			}
		})
	}
}
