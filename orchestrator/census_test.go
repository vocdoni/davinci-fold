package orchestrator

import (
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	davinci "github.com/vocdoni/davinci-zkvm/go-sdk"
	leanimt "github.com/vocdoni/lean-imt-go"
)

func TestVerifyCensusProof(t *testing.T) {
	root, ok := testCensus.Root()
	qt.Assert(t, ok, qt.IsTrue)

	// otherRoot is the root of a census with one more member.
	other, err := leanimt.New(leanimt.PoseidonHasher, leanimt.BigIntEqual, nil, nil, nil)
	qt.Assert(t, err, qt.IsNil)
	qt.Assert(t, other.InsertMany(testCensus.Leaves()), qt.IsNil)
	other.Insert(davinci.PackAddressWeight(new(big.Int).SetBytes(testAddress(testCensusSize)), big.NewInt(1)))
	otherRoot, _ := other.Root()

	member := censusProof(t, 2)
	tests := []struct {
		name    string
		address []byte
		mutate  func(cp *davinci.CensusProof)
		wantErr string
	}{
		{name: "member", address: testAddress(2)},
		{
			name:    "member, upper-case hex without 0x",
			address: testAddress(2),
			mutate: func(cp *davinci.CensusProof) {
				cp.Root = strings.ToUpper(strings.TrimPrefix(cp.Root, "0x"))
				cp.Siblings[0] = strings.ToUpper(cp.Siblings[0][2:])
			},
		},
		{
			name:    "bogus sibling",
			address: testAddress(2),
			mutate:  func(cp *davinci.CensusProof) { cp.Siblings[0] = fr32(big.NewInt(12345)) },
			wantErr: "census proof does not reach the census root",
		},
		{
			name:    "bogus path bit",
			address: testAddress(2),
			mutate:  func(cp *davinci.CensusProof) { cp.Index ^= 1 },
			wantErr: "census proof does not reach the census root",
		},
		{
			name:    "missing sibling",
			address: testAddress(2),
			mutate:  func(cp *davinci.CensusProof) { cp.Siblings = cp.Siblings[1:]; cp.Index >>= 1 },
			wantErr: "census proof does not reach the census root",
		},
		{
			name:    "leaf of another address",
			address: testAddress(3),
			wantErr: "census leaf is for address",
		},
		{
			name:    "root of another census",
			address: testAddress(2),
			mutate:  func(cp *davinci.CensusProof) { cp.Root = fr32(otherRoot) },
			wantErr: "census root mismatch",
		},
		{
			name:    "short hex",
			address: testAddress(2),
			mutate:  func(cp *davinci.CensusProof) { cp.Leaf = "0x1234" },
			wantErr: "census leaf: want 32 bytes, got 2",
		},
		{
			name:    "not a field element",
			address: testAddress(2),
			mutate:  func(cp *davinci.CensusProof) { cp.Siblings[0] = "0x" + strings.Repeat("ff", 32) },
			wantErr: "census sibling 0: 0xffff",
		},
		{
			name:    "path bits above depth",
			address: testAddress(2),
			mutate:  func(cp *davinci.CensusProof) { cp.Index |= 1 << uint(len(cp.Siblings)) },
			wantErr: "path bits above depth",
		},
		{
			name:    "too deep",
			address: testAddress(2),
			mutate:  func(cp *davinci.CensusProof) { cp.Siblings = make([]string, maxCensusDepth+1) },
			wantErr: "depth 62 exceeds 61",
		},
		{
			name:    "short address",
			address: testAddress(2)[:19],
			wantErr: "address must be 20 bytes",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cp := member
			cp.Siblings = append([]string(nil), member.Siblings...)
			if tc.mutate != nil {
				tc.mutate(&cp)
			}
			weight, canonical, err := verifyCensusProof(root, tc.address, cp)
			if tc.wantErr == "" {
				qt.Assert(t, err, qt.IsNil)
				qt.Assert(t, weight.Int64(), qt.Equals, int64(1))
				qt.Assert(t, canonical, qt.DeepEquals, member)
				return
			}
			qt.Assert(t, err, qt.ErrorMatches, ".*"+regexp.QuoteMeta(tc.wantErr)+".*")
		})
	}
}

func TestCheckCensusOrigin(t *testing.T) {
	for origin, wantErr := range map[uint64]string{
		0: "unknown census origin 0",
		1: "",
		2: "",
		3: "",
		4: "census origin 4 (CSP) is not supported",
		5: "unknown census origin 5",
	} {
		err := checkCensusOrigin(origin)
		if wantErr == "" {
			qt.Assert(t, err, qt.IsNil, qt.Commentf("origin %d", origin))
			continue
		}
		qt.Assert(t, err, qt.ErrorMatches, regexp.QuoteMeta(wantErr), qt.Commentf("origin %d", origin))
	}
}

// TestCreateElectionRejectsCSP checks an election the orchestrator cannot
// verify votes for is refused at creation.
func TestCreateElectionRejectsCSP(t *testing.T) {
	c := qt.New(t)
	e, _ := newTestEngine(t)
	defer e.Stop()

	el, _ := testElection(t, 0x20, time.Now().Add(time.Hour))
	el.Config.CensusOrigin = uint64(davinci.CensusOriginCSP)
	c.Assert(e.CreateElection("admin", el), qt.ErrorMatches, ".*CSP.*not supported.*")
	_, err := e.Election(el.ID)
	c.Assert(err, qt.IsNotNil)
}

// TestIngestRejectsNonMember drives a non-member's vote through SubmitVote.
func TestIngestRejectsNonMember(t *testing.T) {
	c := qt.New(t)
	e, _ := newTestEngine(t)
	defer e.Stop()

	el, encKey := testElection(t, 0x21, time.Now().Add(time.Hour))
	c.Assert(e.CreateElection("admin", el), qt.IsNil)

	// A member's proof under an address outside the census.
	sub := makeSub(t, encKey, 0)
	sub.Address = testAddress(testCensusSize)
	_, err := e.SubmitVote(el.ID, sub)
	c.Assert(err, qt.ErrorMatches, "invalid vote: census leaf is for address .*")

	// The member itself is accepted.
	_, err = e.SubmitVote(el.ID, makeSub(t, encKey, 0))
	c.Assert(err, qt.IsNil)
}
