package epochLeader

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/core/state"
	"github.com/wanchain/go-wanchain/core/vm"
	"github.com/wanchain/go-wanchain/crypto"
	"github.com/wanchain/go-wanchain/params"
	"github.com/wanchain/go-wanchain/pos/posdb"
	bn256 "github.com/wanchain/pos/cloudflare"
)

var (
	Big1                                   = big.NewInt(1)
	Big0                                   = big.NewInt(0)
	ErrInvalidRandomProposerSelection      = errors.New("Invalid Random Proposer Selection")                  //Invalid Random Proposer Selection
	ErrInvalidEpochProposerSelection       = errors.New("Invalid Epoch Proposer Selection")                   //Invalid Random Proposer Selection
	ErrInvalidProbabilityfloat2big         = errors.New("Invalid Transform Probability From Float To Bigint") //Invalid Transform Probability From Float To Bigint
	ErrInvalidGenerateCommitment           = errors.New("Invalid Commitment Generation")                      //Invalid Commitment Generation
	ErrInvalidArrayPieceGeneration         = errors.New("Invalid ArrayPiece Generation")                      //Invalid ArrayPiece Generation
	ErrInvalidDleqProofGeneration          = errors.New("Invalid DLEQ Proof Generation")                      //Invalid DLEQ Proof Generation
	ErrInvalidSecretMessageArrayGeneration = errors.New("Invalid Secret Message Array Generation")            //Invalid Secret Message Array Generation
	ErrInvalidSortPublicKeys               = errors.New("Invalid PublicKeys Sort Operation")                  //Invalid PublicKeys Sort Operation
	ErrInvalidSlotLeaderSequenceGeneration = errors.New("Invalid Slot Leader Sequence Generation")            //Invalid Slot Leader Sequence Generation
	ErrInvalidSlotLeaderLocation           = errors.New("Invalid Slot Leader Location")                       //Invalid Slot Leader Location
	ErrInvalidSlotLeaderProofGeneration    = errors.New("Invalid Slot Leader Proof Generation")               //Invalid Slot Leader Proof Generation
)

type Epocher struct {
	rbLeadersDb    *posdb.Db
	epochLeadersDb *posdb.Db
}

func NewEpocher() *Epocher {

	rbdb := posdb.NewDb("rblocaldb")

	epdb := posdb.NewDb("eplocaldb")

	return &Epocher{rbdb, epdb}
}

func (e *Epocher) SelectLeaders(r []byte, ne int, nr int, statedb *state.StateDB, epochId uint64) error {

	pa, err := e.createStakerProbabilityArray(statedb)
	if pa == nil || err != nil {
		return err
	}

	epl := e.GetEpochLeaders(epochId)
	if len(epl) != ne {
		e.epochLeaderSelection(r, ne, pa, epochId)
	}

	rbl := e.GetRBProposerGroup(epochId)
	if len(rbl) != nr {
		e.randomProposerSelection(r, nr, pa, epochId)
	}

	return nil

}

type Proposer struct {
	pubSec256     *ecdsa.PublicKey //staker’s ethereum public key
	pubBn256      *bn256.G1        //staker’s bn256 public key
	probabilities *big.Int
}

type ProposerSorter []Proposer

func newProposerSorter() ProposerSorter {
	ps := make(ProposerSorter, 0)
	return ps
}

//Len()
func (s ProposerSorter) Len() int {
	return len(s)
}

func (s ProposerSorter) Less(i, j int) bool {
	return s[i].probabilities.Cmp(s[j].probabilities) < 0
}

//Swap()
func (s ProposerSorter) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

const Accuracy float64 = 1024.0 //accuracy to magnificate
//wanhumber*locktime*(exp-(t) ),t=(locktime - passedtime/locktime)
func (e *Epocher) generateProblility(pstaker *vm.StakerInfo) (*Proposer, error) {

	amount := big.NewInt(0).Div(pstaker.Amount, big.NewInt(params.Wan)).Int64()
	lockTime := pstaker.LockTime
	leftTimePercent := (float64(int64(lockTime)-(time.Now().Unix()-pstaker.StakingTime)) / float64(lockTime))
	pb := float64(amount) * float64(lockTime) * math.Exp(-leftTimePercent) * Accuracy

	gb := new(bn256.G1)
	_, err := gb.Unmarshal(pstaker.PubBn256)
	if err != nil {
		return nil, err
	}

	p := &Proposer{
		pubSec256:     crypto.ToECDSAPub(pstaker.PubSec256),
		pubBn256:      gb,
		probabilities: big.NewInt(int64(pb)),
	}

	return p, nil

}

func (e *Epocher) createStakerProbabilityArray(statedb *state.StateDB) (ProposerSorter, error) {

	if statedb == nil {
		return nil, vm.ErrUnknown
	}

	listAddr := vm.StakersInfoAddr
	ps := newProposerSorter()

	statedb.ForEachStorageByteArray(listAddr, func(key common.Hash, value []byte) bool {

		//fmt.Println("for each get data",value)
		staker := vm.StakerInfo{}
		err := json.Unmarshal(value, &staker)
		if err != nil {
			return false
		}

		pitem, err := e.generateProblility(&staker)
		if err != nil {
			return false
		}

		ps = append(ps, *pitem)

		return true
	})

	sort.Sort(ProposerSorter(ps))

	for idx, _ := range ps {
		if idx == 0 {
			continue
		}
		fmt.Println("probility" + strconv.Itoa(idx) + "  	" + common.ToHex(ps[idx].probabilities.Bytes()))
	}

	fmt.Println("------------------------------------------------------------------------------------------")
	for idx, _ := range ps {
		if idx == 0 {
			continue
		}

		ps[idx].probabilities = big.NewInt(0).Add(ps[idx].probabilities, ps[idx-1].probabilities)

		fmt.Println("after sum probility" + strconv.Itoa(idx) + "  	" + common.ToHex(ps[idx].probabilities.Bytes()))
	}

	return ps, nil
}

//samples nr random proposers by random number r（Random Beacon) from PublicKeys based on proportion of Probabilities
func (e *Epocher) epochLeaderSelection(r []byte, nr int, ps ProposerSorter, epochId uint64) error {
	if r == nil || nr <= 0 || len(ps) == 0 {
		return ErrInvalidRandomProposerSelection
	}

	//the last one is total properties
	tp := ps[len(ps)-1].probabilities

	fmt.Println("tp=" + common.ToHex(tp.Bytes()))

	var Byte0 = []byte{byte(0)}
	var buffer bytes.Buffer
	buffer.Write(Byte0)
	buffer.Write(r)
	r0 := buffer.Bytes()       //r0 = 0||r
	cr := crypto.Keccak256(r0) //cr = hash(r0)

	//randomProposerPublicKeys := make([]*ecdsa.PublicKey, 0)  //store the selected publickeys

	for i := 0; i < nr; i++ {

		crBig := new(big.Int).SetBytes(cr)
		crBig = crBig.Mod(crBig, tp) //cr_big = cr mod tp

		fmt.Println("epoch leader mod tp=" + common.ToHex(crBig.Bytes()))
		//select pki whose probability bigger than cr_big left
		idx := sort.Search(len(ps), func(i int) bool { return ps[i].probabilities.Cmp(crBig) > 0 })

		if idx == len(ps) {
			idx = len(ps) - 1
		}

		fmt.Println("select probility" + strconv.Itoa(idx) + " " + common.ToHex(ps[idx].probabilities.Bytes()))
		//randomProposerPublicKeys = append(randomProposerPublicKeys, ps[idx + 1].pubSec256)
		e.epochLeadersDb.PutWithIndex(epochId, uint64(i), "", crypto.FromECDSAPub(ps[idx].pubSec256))

		cr = crypto.Keccak256(cr)
	}

	return nil
}

//*bn256.G1
//samples ne epoch leaders by random number r from PublicKeys based on proportion of Probabilities
func (e *Epocher) randomProposerSelection(r []byte, nr int, ps ProposerSorter, epochId uint64) error {
	if r == nil || nr <= 0 || len(ps) == 0 {
		return ErrInvalidEpochProposerSelection
	}

	//the last one is total properties
	tp := ps[len(ps)-1].probabilities

	var Byte1 = []byte{byte(1)}
	var buffer bytes.Buffer
	buffer.Write(Byte1)
	buffer.Write(r)
	r1 := buffer.Bytes()       //r1 = 1||r
	cr := crypto.Keccak256(r1) //cr = hash(r1)

	for i := 0; i < nr; i++ {

		crBig := new(big.Int).SetBytes(cr)
		crBig = crBig.Mod(crBig, tp) //cr_big = cr mod tp

		fmt.Println("mod tp=" + common.ToHex(crBig.Bytes()))

		//select pki whose probability bigger than cr_big left
		idx := sort.Search(len(ps), func(i int) bool { return ps[i].probabilities.Cmp(crBig) > 0 })

		if idx == len(ps) {
			idx = len(ps) - 1
		}

		e.rbLeadersDb.PutWithIndex(epochId, uint64(i), "", ps[idx].pubBn256.Marshal())
		//EpochLeaderBn256G1s = append(EpochLeaderBn256G1s, ps[idx + 1].pubBn256)

		cr = crypto.Keccak256(cr)
	}

	return nil
}

//get epochLeaders of epochID in localdb
func (e *Epocher) GetEpochLeaders(epochID uint64) [][]byte {

	ksarray := e.epochLeadersDb.GetStorageByteArray(epochID)

	return ksarray

}

//get rbLeaders of epochID in localdb
func (e *Epocher) GetRBProposerGroup(epochID uint64) []bn256.G1 {

	ksarray := e.rbLeadersDb.GetStorageByteArray(epochID)
	g1ksArray := make([]bn256.G1, 0)

	for _, ks := range ksarray {

		gb := new(bn256.G1)
		_, err := gb.Unmarshal(ks)
		if err != nil {
			return nil
		}

		g1ksArray = append(g1ksArray, *gb)
	}

	return g1ksArray
}