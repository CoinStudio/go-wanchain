package slotleader

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/wanchain/go-wanchain/consensus"

	"github.com/wanchain/go-wanchain/accounts/keystore"
	"github.com/wanchain/go-wanchain/core"
	"github.com/wanchain/go-wanchain/core/state"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/core/vm"
	"github.com/wanchain/go-wanchain/functrace"
	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/pos/posconfig"
	"github.com/wanchain/go-wanchain/pos/posdb"
	"github.com/wanchain/go-wanchain/pos/util/convert"

	lru "github.com/hashicorp/golang-lru"
	"github.com/wanchain/go-wanchain/rpc"

	"github.com/wanchain/go-wanchain/crypto"
	"github.com/wanchain/go-wanchain/pos/uleaderselection"
	"github.com/wanchain/go-wanchain/pos/util"
	"github.com/wanchain/go-wanchain/rlp"
)

const lengthPublicKeyBytes = 65
const (
	StageTwoProofCount = 2
	EpochLeaders       = "epochLeaders"
	SecurityMsg        = "securityMsg"
	CR                 = "cr"
	SlotLeader         = "slotLeader"
)
const (
	slotLeaderSelectionInit = iota + 1 //1
	//Ready to start slot leader selection stage1
	slotLeaderSelectionStage1 = iota + 1 //2
	//Slot leader selection stage1 finish
	slotLeaderSelectionStage2        = iota + 1 //3
	slotLeaderSelectionStage3        = iota + 1 //4
	slotLeaderSelectionStageFinished = iota + 1 //5
)

var (
	errorRetry = 3
)

type SLS struct {
	workingEpochID uint64
	workStage      int
	rc             *rpc.Client
	key            *keystore.Key
	stateDbTest    *state.StateDB

	epochLeadersArray []string            // len(pki)=65 hex.EncodeToString
	epochLeadersMap   map[string][]uint64 // key: pki value: []uint64 the indexes of this pki. hex.EncodeToString

	slotLeadersPtrArray  [posconfig.SlotCount]*ecdsa.PublicKey
	slotLeadersIndex     [posconfig.SlotCount]uint64
	epochLeadersPtrArray [posconfig.EpochLeaderCount]*ecdsa.PublicKey
	// true: can be used to slot leader false: can not be used to slot leader
	validEpochLeadersIndex [posconfig.EpochLeaderCount]bool

	stageOneMi       [posconfig.EpochLeaderCount]*ecdsa.PublicKey
	stageTwoAlphaPKi [posconfig.EpochLeaderCount][posconfig.EpochLeaderCount]*ecdsa.PublicKey
	stageTwoProof    [posconfig.EpochLeaderCount][StageTwoProofCount]*big.Int //[0]: e; [1]:Z

	slotCreateStatus       map[uint64]bool
	slotCreateStatusLockCh chan int

	blockChain *core.BlockChain

	epochLeadersPtrArrayGenesis [posconfig.EpochLeaderCount]*ecdsa.PublicKey
	stageOneMiGenesis           [posconfig.EpochLeaderCount]*ecdsa.PublicKey
	stageTwoAlphaPKiGenesis     [posconfig.EpochLeaderCount][posconfig.EpochLeaderCount]*ecdsa.PublicKey
	stageTwoProofGenesis        [posconfig.EpochLeaderCount][StageTwoProofCount]*big.Int //[0]: e; [1]:Z
	randomGenesis               *big.Int
	smaGenesis                  [posconfig.EpochLeaderCount]*ecdsa.PublicKey

	sendTransactionFn SendTxFn
}

var slotLeaderSelection *SLS
var APkiCache *lru.ARCCache

type Pack struct {
	Proof    [][]byte
	ProofMeg [][]byte
}

func GetSlotLeaderSelection() *SLS {
	return slotLeaderSelection
}
func (s *SLS) GetLocalPublicKey() (*ecdsa.PublicKey, error) {
	return s.getLocalPublicKey()
}

func (s *SLS) GetEpochLeadersPK(epochID uint64) []*ecdsa.PublicKey {
	return s.getEpochLeadersPK(epochID)
}

func (s *SLS) GetSlotCreateStatusByEpochID(epochID uint64) bool {
	s.slotCreateStatusLockCh <- 1
	_, ok := s.slotCreateStatus[epochID]
	<-s.slotCreateStatusLockCh
	return ok
}

func (s *SLS) GetSlotLeader(epochID uint64, slotID uint64) (slotLeader *ecdsa.PublicKey, err error) {
	//todo maybe the start epochid is not 0
	if epochID == 0 {
		b, err := hex.DecodeString(posconfig.GenesisPK)
		if err != nil {
			return nil, vm.ErrInvalidGenesisPk
		}
		return crypto.ToECDSAPub(b), nil
	}

	if slotID >= posconfig.SlotCount {
		return nil, vm.ErrSlotIDOutOfRange
	}

	// read from memory
	s.slotCreateStatusLockCh <- 1
	created, ok := s.slotCreateStatus[epochID]
	<-s.slotCreateStatusLockCh
	if ok && created {
		return s.slotLeadersPtrArray[slotID], nil
	}
	// read from local db
	for i := 0; i < posconfig.SlotCount; i++ {
		pkByte, err := posdb.GetDb().GetWithIndex(epochID, uint64(i), SlotLeader)
		if err != nil {
			return nil, vm.ErrSlotLeaderGroupNotReady
		}
		s.slotLeadersPtrArray[i] = crypto.ToECDSAPub(pkByte)
	}
	s.slotCreateStatusLockCh <- 1
	s.slotCreateStatus[epochID] = true
	<-s.slotCreateStatusLockCh

	return s.slotLeadersPtrArray[slotID], nil
}

func (s *SLS) GetSma(epochID uint64) (ret []*ecdsa.PublicKey, isGenesis bool, err error) {
	return s.getSMAPieces(epochID)
}

func SlsInit() {
	var err error
	APkiCache, err = lru.NewARC(1000)
	if err != nil || APkiCache == nil {
		log.SyslogErr("APkiCache failed")
	}
	slotLeaderSelection = &SLS{}
	slotLeaderSelection.epochLeadersMap = make(map[string][]uint64)
	slotLeaderSelection.epochLeadersArray = make([]string, 0)
	slotLeaderSelection.slotCreateStatus = make(map[uint64]bool)
	slotLeaderSelection.slotCreateStatusLockCh = make(chan int, 1)
	s := slotLeaderSelection
	s.randomGenesis = big.NewInt(1)
	epoch0Leaders := s.getEpoch0LeadersPK()
	for index, value := range epoch0Leaders {
		s.epochLeadersPtrArrayGenesis[index] = value
	}

	alphas := make([]*big.Int, 0)
	for _, value := range epoch0Leaders {
		tempInt := new(big.Int).SetInt64(0)
		tempInt.SetBytes(crypto.Keccak256(crypto.FromECDSAPub(value)))
		alphas = append(alphas, tempInt)
	}

	for i := 0; i < posconfig.EpochLeaderCount; i++ {

		// AlphaPK  stage1Genesis
		mi0 := new(ecdsa.PublicKey)
		mi0.Curve = crypto.S256()
		mi0.X, mi0.Y = crypto.S256().ScalarMult(s.epochLeadersPtrArrayGenesis[i].X, s.epochLeadersPtrArrayGenesis[i].Y,
			alphas[i].Bytes())
		s.stageOneMiGenesis[i] = mi0

		// G
		BasePoint := new(ecdsa.PublicKey)
		BasePoint.Curve = crypto.S256()
		BasePoint.X, BasePoint.Y = crypto.S256().ScalarBaseMult(big.NewInt(1).Bytes())

		// alphaG SMAGenesis
		smaPiece := new(ecdsa.PublicKey)
		smaPiece.Curve = crypto.S256()
		smaPiece.X, smaPiece.Y = crypto.S256().ScalarMult(BasePoint.X, BasePoint.Y, alphas[i].Bytes())
		s.smaGenesis[i] = smaPiece

		for j := 0; j < posconfig.EpochLeaderCount; j++ {
			// AlphaIPki stage2Genesis, used to verify genesis proof
			alphaIPkj := new(ecdsa.PublicKey)
			alphaIPkj.Curve = crypto.S256()
			alphaIPkj.X, alphaIPkj.Y = crypto.S256().ScalarMult(s.epochLeadersPtrArrayGenesis[j].X,
				s.epochLeadersPtrArrayGenesis[j].Y, alphas[i].Bytes())

			s.stageTwoAlphaPKiGenesis[i][j] = alphaIPkj
		}

	}

	epochLeadersPreHexStr := make([]string, 0)
	for _, value := range s.epochLeadersPtrArrayGenesis {
		epochLeadersPreHexStr = append(epochLeadersPreHexStr, hex.EncodeToString(crypto.FromECDSAPub(value)))
	}
	log.Debug("slot_leader_selection:init", "genesis epoch leaders", epochLeadersPreHexStr)

	smaPiecesHexStr := make([]string, 0)
	for _, value := range s.smaGenesis {
		smaPiecesHexStr = append(smaPiecesHexStr, hex.EncodeToString(crypto.FromECDSAPub(value)))
	}
	log.Debug("slot_leader_selection:init", "genesis sma pieces", smaPiecesHexStr)
	log.SyslogInfo("SLS SlsInit success")

}

func (s *SLS) getSlotLeaderStage2TxIndexes(epochID uint64) (indexesSentTran []bool, err error) {
	var ret [posconfig.EpochLeaderCount]bool
	stateDb, err := s.getCurrentStateDb()
	if err != nil {
		return ret[:], err
	}

	slotLeaderPrecompileAddr := vm.GetSlotLeaderSCAddress()

	keyHash := vm.GetSlotLeaderStage2IndexesKeyHash(convert.Uint64ToBytes(epochID))

	log.Debug(fmt.Sprintf("getSlotLeaderStage2TxIndexes:try to get stateDB addr:%s, key:%s",
		slotLeaderPrecompileAddr.Hex(), keyHash.Hex()))

	data := stateDb.GetStateByteArray(slotLeaderPrecompileAddr, keyHash)

	if data == nil {
		return ret[:], vm.ErrNoTx2TransInDB
	}

	err = rlp.DecodeBytes(data, &ret)
	if err != nil {
		return ret[:], vm.ErrNoTx2TransInDB
	}
	return ret[:], nil
}

func (s *SLS) getAlpha(epochID uint64, selfIndex uint64) (*big.Int, error) {
	if posconfig.SelfTestMode {
		ret := big.NewInt(123)
		return ret, nil
	}
	buf, err := posdb.GetDb().GetWithIndex(epochID, selfIndex, "alpha")
	if err != nil {
		return nil, err
	}

	var alpha = big.NewInt(0).SetBytes(buf)
	return alpha, nil
}

func (s *SLS) getLocalPublicKey() (*ecdsa.PublicKey, error) {
	if s.key == nil || s.key.PrivateKey == nil {
		log.SyslogErr("SLS", "getLocalPublicKey", vm.ErrInvalidLocalPublicKey.Error())
		return nil, vm.ErrInvalidLocalPublicKey
	}
	return &s.key.PrivateKey.PublicKey, nil
}

func (s *SLS) getLocalPrivateKey() (*ecdsa.PrivateKey, error) {
	return s.key.PrivateKey, nil
}

func (s *SLS) getEpochLeaders(epochID uint64) [][]byte {
	//test := false
	if posconfig.SelfTestMode {
		//test: generate test publicKey
		epochLeaderAllBytes, err := posdb.GetDb().Get(epochID, EpochLeaders)
		if err != nil {
			return nil
		}
		piecesCount := len(epochLeaderAllBytes) / lengthPublicKeyBytes
		ret := make([][]byte, 0)
		var pubKeyByte []byte
		for i := 0; i < piecesCount; i++ {
			if i < piecesCount-1 {
				pubKeyByte = epochLeaderAllBytes[i*lengthPublicKeyBytes : (i+1)*lengthPublicKeyBytes]
			} else {
				pubKeyByte = epochLeaderAllBytes[i*lengthPublicKeyBytes:]
			}
			ret = append(ret, pubKeyByte)
		}
		return ret
	} else {
		type epoch interface {
			GetEpochLeaders(epochID uint64) [][]byte
		}

		selector := util.GetEpocherInst() //TODO:CHECK INIT

		if selector == nil {
			return nil
		}

		epochLeaders := selector.GetEpochLeaders(epochID)
		if epochLeaders != nil {
			log.Debug(fmt.Sprintf("getEpochLeaders called return len(epochLeaders):%d", len(epochLeaders)))
		}
		return epochLeaders
	}
}

func (s *SLS) getEpochLeadersPK(epochID uint64) []*ecdsa.PublicKey {
	bufs := s.getEpochLeaders(epochID)

	pks := make([]*ecdsa.PublicKey, len(bufs))
	for i := 0; i < len(bufs); i++ {
		pks[i] = crypto.ToECDSAPub(bufs[i])
	}

	return pks
}

func (s *SLS) getPreEpochLeadersPK(epochID uint64) ([]*ecdsa.PublicKey, error) {
	if epochID == 0 {
		return s.getEpoch0LeadersPK(), nil
	}

	pks := s.getEpochLeadersPK(epochID - 1)
	if len(pks) == 0 {
		log.Warn("Can not found pre epoch leaders return epoch 0", "epochIDPre", epochID-1)
		return s.getEpoch0LeadersPK(), vm.ErrInvalidPreEpochLeaders
	}

	return pks, nil
}

func (s *SLS) getEpoch0LeadersPK() []*ecdsa.PublicKey {
	pks := make([]*ecdsa.PublicKey, posconfig.EpochLeaderCount)
	for i := 0; i < posconfig.EpochLeaderCount; i++ {
		pkBuf, err := hex.DecodeString(posconfig.GenesisPK)
		if err != nil {
			// epoch 0 use the genesis pK to propose block, since it comes from configuration
			// If the configuration has error, system should not continue.
			panic("posconfig.GenesisPK is Error")
		}
		pks[i] = crypto.ToECDSAPub(pkBuf)
	}
	return pks
}

// isLocalPkInPreEpochLeaders check if local pk is in pre epoch leader.
// If get pre epoch leader length is 0, return true,err to use epoch 0 info
func (s *SLS) isLocalPkInPreEpochLeaders(epochID uint64) (canBeContinue bool, err error) {

	localPk, err := s.getLocalPublicKey()
	if err != nil {
		log.Error("SLS.IsLocalPkInPreEpochLeaders getLocalPublicKey error", "error", err)
		return false, err
	}

	if epochID == 0 {
		for _, value := range s.epochLeadersPtrArrayGenesis {
			if util.PkEqual(localPk, value) {
				return true, nil
			}
		}
		return false, nil
	}

	prePks, err := s.getPreEpochLeadersPK(epochID)
	if err != nil {
		return true, vm.ErrInvalidPreEpochLeaders
	}

	for i := 0; i < len(prePks); i++ {
		if util.PkEqual(localPk, prePks[i]) {
			return true, nil
		}
	}
	return false, nil
}

func (s *SLS) isLocalPkInCurrentEpochLeaders() bool {
	selfPublicKey, _ := s.getLocalPublicKey()
	var inEpochLeaders bool
	_, inEpochLeaders = s.epochLeadersMap[hex.EncodeToString(crypto.FromECDSAPub(selfPublicKey))]
	if inEpochLeaders {
		return true
	}
	log.Debug("isLocalPkInCurrentEpochLeaders", "local public key:",
		hex.EncodeToString(crypto.FromECDSAPub(selfPublicKey)))
	log.Debug("isLocalPkInCurrentEpochLeaders", "s.epochLeadersMap:", s.epochLeadersMap)
	return false
}

func (s *SLS) clearData() {
	s.epochLeadersArray = make([]string, 0)
	s.epochLeadersMap = make(map[string][]uint64)

	s.slotCreateStatusLockCh <- 1
	s.slotCreateStatus = make(map[uint64]bool)
	<-s.slotCreateStatusLockCh

	for i := 0; i < posconfig.EpochLeaderCount; i++ {
		s.epochLeadersPtrArray[i] = nil
		s.validEpochLeadersIndex[i] = true

		s.stageOneMi[i] = nil

		for j := 0; j < posconfig.EpochLeaderCount; j++ {
			s.stageTwoAlphaPKi[i][j] = nil
		}
		for k := 0; k < StageTwoProofCount; k++ {
			s.stageTwoProof[i][k] = nil
		}
	}

	for i := 0; i < posconfig.SlotCount; i++ {
		s.slotLeadersPtrArray[i] = nil
	}

	for i := 0; i < posconfig.SlotCount; i++ {
		s.slotLeadersIndex[i] = 0
	}
}

func (s *SLS) dumpData() {

	s.dumpPreEpochLeaders()
	s.dumpCurrentEpochLeaders()
	s.dumpSlotLeaders()
	s.dumpLocalPublicKey()
	s.dumpLocalPublicKeyIndex()
}

func (s *SLS) dumpPreEpochLeaders() {
	log.Debug("\n")
	currentEpochID := s.getWorkingEpochID()
	log.Debug("dumpPreEpochLeaders", "currentEpochID", currentEpochID)
	if currentEpochID == 0 {
		return
	}

	preEpochLeaders := s.getEpochLeaders(currentEpochID - 1)
	for i := 0; i < len(preEpochLeaders); i++ {
		log.Debug("dumpPreEpochLeaders", "index", i, "preEpochLeader", hex.EncodeToString(preEpochLeaders[i]))
	}

	log.Debug("\n")
}
func (s *SLS) dumpCurrentEpochLeaders() {
	log.Debug("\n")
	currentEpochID := s.getWorkingEpochID()
	log.Debug("dumpCurrentEpochLeaders", "currentEpochID", currentEpochID)
	if currentEpochID == 0 {
		return
	}

	for index, value := range s.epochLeadersPtrArray {
		log.Debug("dumpCurrentEpochLeaders", "index", index, "curEpochLeader",
			hex.EncodeToString(crypto.FromECDSAPub(value)))
	}

}

func (s *SLS) dumpSlotLeaders() {
	log.Debug("\n")
	currentEpochID := s.getWorkingEpochID()
	log.Debug("dumpSlotLeaders", "currentEpochID", currentEpochID)
	if currentEpochID == 0 {
		return
	}

	for index, value := range s.slotLeadersPtrArray {
		log.Debug("dumpSlotLeaders", "index", s.slotLeadersIndex[index], "curSlotLeader",
			hex.EncodeToString(crypto.FromECDSAPub(value)))
	}

}

func (s *SLS) dumpLocalPublicKey() {
	log.Debug("\n")
	localPublicKey, _ := s.getLocalPublicKey()
	log.Debug("dumpLocalPublicKey", "current Local publickey", hex.EncodeToString(crypto.FromECDSAPub(localPublicKey)))

}

func (s *SLS) dumpLocalPublicKeyIndex() {
	log.Debug("\n")
	localPublicKey, _ := s.getLocalPublicKey()
	localPublicKeyByte := crypto.FromECDSAPub(localPublicKey)
	log.Debug("current Local publickey", "indexes in current epochLeaders",
		s.epochLeadersMap[hex.EncodeToString(localPublicKeyByte)])

}

func (s *SLS) buildEpochLeaderGroup(epochID uint64) {
	functrace.Enter()
	// build Array and map
	data := s.getEpochLeaders(epochID)
	if data == nil {
		log.SyslogErr("SLS", "buildEpochLeaderGroup", "no epoch leaders", "epochID", epochID)
		// no epoch leaders, it leads that no one send SMA stage1 and stage2 transaction.
		// comment panic, because let node live to used for others node synchronization.
		//panic("No epoch leaders")
		return
	}
	for index, value := range data {
		s.epochLeadersArray = append(s.epochLeadersArray, hex.EncodeToString(value))
		s.epochLeadersMap[hex.EncodeToString(value)] = append(s.epochLeadersMap[hex.EncodeToString(value)],
			uint64(index))
		s.epochLeadersPtrArray[index] = crypto.ToECDSAPub(value)
	}
	functrace.Exit()
}

func (s *SLS) getRandom(block *types.Block, epochID uint64) (ret *big.Int, err error) {
	// If db is nil, use current stateDB
	var db *state.StateDB
	if block == nil {
		db, err = s.getCurrentStateDb()
		if err != nil {
			log.SyslogErr("SLS.getRandom getStateDb return error, use a default value", "epochID", epochID)
			rb := big.NewInt(1)
			return rb, nil
		}
	} else {
		db, err = s.blockChain.StateAt(s.blockChain.GetBlockByHash(block.ParentHash()).Root())
		if err != nil {
			log.SyslogErr("Update stateDb error in SLS.updateToLastStateDb", "error", err.Error())
			rb := big.NewInt(1)
			return rb, nil
		}
	}

	rb := vm.GetR(db, epochID)
	if rb == nil {
		log.SyslogErr("vm.GetR return nil, use a default value", "epochID", epochID)
		rb = big.NewInt(1)
	}
	return rb, nil
}

// getSMAPieces can get the SMA info generate in pre epoch.
// It had been +1 when save into db, so do not -1 in get.
func (s *SLS) getSMAPieces(epochID uint64) (ret []*ecdsa.PublicKey, isGenesis bool, err error) {
	piecesPtr := make([]*ecdsa.PublicKey, 0)
	if epochID == uint64(0) {
		return s.smaGenesis[:], true, nil
	} else {
		// pieces: alpha[1]*G, alpha[2]*G, .....
		pieces, err := posdb.GetDb().Get(epochID, SecurityMsg)
		if err != nil {
			log.Warn("getSMAPieces error use epoch 0 SMA", "epochID", epochID, "SecurityMsg", SecurityMsg)
			return s.smaGenesis[:], true, nil
		}

		piecesCount := len(pieces) / lengthPublicKeyBytes
		var pubKeyByte []byte
		for i := 0; i < piecesCount; i++ {
			if i < piecesCount-1 {
				pubKeyByte = pieces[i*lengthPublicKeyBytes : (i+1)*lengthPublicKeyBytes]
			} else {
				pubKeyByte = pieces[i*lengthPublicKeyBytes:]
			}
			piecesPtr = append(piecesPtr, crypto.ToECDSAPub(pubKeyByte))
		}
		return piecesPtr, false, nil
	}
}

func (s *SLS) generateSlotLeadsGroup(epochID uint64) error {
	epochIDGet := epochID
	// get pre sma
	piecesPtr, isGenesis, _ := s.getSMAPieces(epochIDGet)
	canBeContinue, err := s.isLocalPkInPreEpochLeaders(epochID)
	if !canBeContinue {
		log.Warn("Local node is not in pre epoch leaders at generateSlotLeadsGroup", "epochID", epochID)
		return nil
	}
	if (err != nil && epochID > 1) || isGenesis {
		if !isGenesis {
			log.Warn("Can not find pre epoch SMA or not in Pre epoch leaders, use epoch 0.", "curEpochID", epochID,
				"preEpochID", epochID-1)
		}
		epochIDGet = 0
	}
	// get random
	random, err := s.getRandom(nil, epochIDGet)
	if err != nil {
		return vm.ErrInvalidRandom
	}
	log.Debug("generateSlotLeadsGroup", "Random got", hex.EncodeToString(random.Bytes()))

	// return slot leaders pointers.
	slotLeadersPtr := make([]*ecdsa.PublicKey, 0)
	var epochLeadersPtrArray []*ecdsa.PublicKey
	if epochIDGet == 0 {
		epochLeadersPtrArray = s.getEpoch0LeadersPK()
	} else {
		epochLeadersPtrArray, err = s.getPreEpochLeadersPK(epochIDGet)
		if err != nil {
			log.Warn(err.Error())
		}
	}

	if len(epochLeadersPtrArray) != posconfig.EpochLeaderCount {
		log.Error("SLS", "Fail to get epoch leader", epochIDGet)
		return fmt.Errorf("fail to get epochLeader:%d", epochIDGet)
	}

	slotLeadersPtr, _, slotLeadersIndex, err := uleaderselection.GenerateSlotLeaderSeqAndIndex(piecesPtr[:],
		epochLeadersPtrArray[:], random.Bytes(), posconfig.SlotCount, epochID)
	if err != nil {
		log.SyslogErr("generateSlotLeadsGroup", "error", err.Error())
		return err
	}

	// insert slot address to local DB
	for index, val := range slotLeadersPtr {
		_, err = posdb.GetDb().PutWithIndex(uint64(epochID), uint64(index), SlotLeader, crypto.FromECDSAPub(val))
		if err != nil {
			log.SyslogErr("generateSlotLeadsGroup:PutWithIndex", "error", err.Error())
			return err
		}
	}

	for index, val := range slotLeadersPtr {
		s.slotLeadersPtrArray[index] = val
	}

	for index, value := range slotLeadersIndex {
		s.slotLeadersIndex[index] = value
	}

	s.slotCreateStatusLockCh <- 1
	s.slotCreateStatus[epochID] = true
	<-s.slotCreateStatusLockCh
	log.SyslogInfo("generateSlotLeadsGroup success")

	s.dumpData()
	return nil
}

// create alpha1*pki,alpha1*PKi,alphaN*PKi,...
// used to create security message.
func (s *SLS) buildSecurityPieces(epochID uint64) (pieces []*ecdsa.PublicKey, err error) {

	selfPk, err := s.getLocalPublicKey()
	if err != nil {
		return nil, err
	}

	indexes, exist := s.epochLeadersMap[hex.EncodeToString(crypto.FromECDSAPub(selfPk))]
	if exist == false {
		log.Warn(fmt.Sprintf("%v not in epoch leaders", hex.EncodeToString(crypto.FromECDSAPub(selfPk))))
		return nil, nil
	}

	selfPkReceivePiecesMap := make(map[uint64][]*ecdsa.PublicKey, 0)
	for _, selfIndex := range indexes {
		for i := 0; i < len(s.epochLeadersArray); i++ {
			if (s.stageTwoAlphaPKi[i][selfIndex] != nil) && (s.validEpochLeadersIndex[i]) {
				selfPkReceivePiecesMap[selfIndex] = append(selfPkReceivePiecesMap[selfIndex],
					s.stageTwoAlphaPKi[i][selfIndex])
			}
		}
	}
	piece := make([]*ecdsa.PublicKey, 0)
	piece = selfPkReceivePiecesMap[indexes[0]]
	// the value in selfPk Received Pieces Map should be same,so we can return the first one.
	return piece, nil
}

func (s *SLS) collectStagesData(epochID uint64) (err error) {
	indexesSentTran, err := s.getSlotLeaderStage2TxIndexes(epochID)
	log.Debug("collectStagesData", "indexesSentTran", indexesSentTran)
	if err != nil {
		log.SyslogErr("collectStagesData", "indexesSentTran", vm.ErrCollectTxData.Error())
		return vm.ErrCollectTxData
	}
	for i := 0; i < posconfig.EpochLeaderCount; i++ {

		if !indexesSentTran[i] {
			s.validEpochLeadersIndex[i] = false
			continue
		}
		// no need get current stateDB, because in getSlotLeaderStage2TxIndexes, have got the current db in stateDb.
		statedb, _ := s.getCurrentStateDb()
		alphaPki, proof, err := vm.GetStage2TxAlphaPki(statedb, epochID, uint64(i))
		if err != nil {
			log.SyslogErr("GetStage2TxAlphaPki", "error", err.Error(), "index", i)
			s.validEpochLeadersIndex[i] = false
			continue
		}

		if (len(alphaPki) != posconfig.EpochLeaderCount) || (len(proof) != StageTwoProofCount) {
			log.SyslogErr("GetStage2TxAlphaPki", "error", "len(alphaPkis) or len(proofs) is wrong.", "index", i)
			s.validEpochLeadersIndex[i] = false
		} else {
			for j := 0; j < posconfig.EpochLeaderCount; j++ {
				s.stageTwoAlphaPKi[i][j] = alphaPki[j]
			}

			for j := 0; j < StageTwoProofCount; j++ {
				s.stageTwoProof[i][j] = proof[j]
			}
		}
	}
	return nil
}

func (s *SLS) generateSecurityMsg(epochID uint64, PrivateKey *ecdsa.PrivateKey) error {
	if !s.isLocalPkInCurrentEpochLeaders() {
		log.Debug("generateSecurityMsg", "input public key",
			hex.EncodeToString(crypto.FromECDSAPub(&PrivateKey.PublicKey)))
		return vm.ErrPkNotInCurrentEpochLeadersGroup
	}
	// collect data
	err := s.collectStagesData(epochID)
	if err != nil {
		return vm.ErrCollectTxData
	}

	// build security self pieces. alpha1*pki, alpha2*pk2, alpha3*pk3....
	ArrayPiece, err := s.buildSecurityPieces(epochID)
	if err != nil {
		log.Warn("generateSecurityMsg:buildSecurityPieces", "error", err.Error())
		return err
	}

	smasPtr := make([]*ecdsa.PublicKey, 0)
	var smasBytes bytes.Buffer

	smasPtr, err = uleaderselection.GenerateSMA(PrivateKey, ArrayPiece)
	if err != nil {
		log.Error("generateSecurityMsg:GenerateSMA", "error", err.Error())
		return err
	}
	for _, value := range smasPtr {
		smasBytes.Write(crypto.FromECDSAPub(value))
		log.Debug(fmt.Sprintf("epochID+1 = %d set security message is %v\n", epochID+1,
			hex.EncodeToString(crypto.FromECDSAPub(value))))
	}
	_, err = posdb.GetDb().Put(uint64(epochID+1), SecurityMsg, smasBytes.Bytes())
	if err != nil {
		log.SyslogErr("generateSecurityMsg:Put", "error", err.Error())
		return err
	}
	return nil
}

// used for stage2 payload
// stage2 tx payload 1(alpha * Pk1, alpha * Pk2, ..., alpha * Pkn)
// stage2 tx payload 2 proof pai[i]
// []*ecdsa : payload1 []*big.Int payload2

func (s *SLS) buildArrayPiece(epochID uint64, selfIndex uint64) ([]*ecdsa.PublicKey, []*big.Int, error) {

	// get alpha
	alpha, err := s.getAlpha(epochID, selfIndex)
	if err != nil {
		return nil, nil, err
	}

	publicKeys := s.epochLeadersPtrArray[:]
	for i := 0; i < len(publicKeys); i++ {
		if publicKeys[i] == nil {
			log.SyslogErr("epochLeader is not ready")
			return nil, nil, errors.New("epochLeader is not ready")
		}
	}
	_, ArrayPiece, proof, err := uleaderselection.GenerateArrayPiece(publicKeys, alpha)
	return ArrayPiece, proof, err
}

func (s *SLS) buildStage2TxPayload(epochID uint64, selfIndex uint64) ([]byte, error) {
	var selfPk *ecdsa.PublicKey
	var err error
	if posconfig.SelfTestMode {
		selfPk = s.epochLeadersPtrArray[selfIndex]
	} else {
		selfPk, err = s.getLocalPublicKey()
		if err != nil {
			return nil, err
		}
	}

	alphaPki, proof, err := s.buildArrayPiece(epochID, selfIndex)
	if err != nil {
		return nil, err
	}
	buf, err := vm.RlpPackStage2DataForTx(epochID, selfIndex, selfPk, alphaPki, proof, vm.GetSlotLeaderScAbiString())

	return buf, err
}

// GetChainReader can get a simple reader interface of blockchain
func (s *SLS) GetChainReader() consensus.ChainReader {
	return s.blockChain
}
