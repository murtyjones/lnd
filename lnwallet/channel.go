package lnwallet

import (
	"sync"

	"li.lan/labs/plasma/chainntfs"
	"li.lan/labs/plasma/channeldb"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/txsort"
)

const (
	// TODO(roasbeef): make not random value
	MaxPendingPayments = 10

	// TODO(roasbeef): remove these and use the one's defined in txscript
	// within testnet-L.
	SequenceLockTimeSeconds = uint32(1 << 22)
	SequenceLockTimeMask    = uint32(0x0000ffff)
)

// LightningChannel...
// TODO(roasbeef): future peer struct should embed this struct
type LightningChannel struct {
	wallet        *LightningWallet
	channelEvents *chainntnfs.ChainNotifier

	// TODO(roasbeef): Stores all previous R values + timeouts for each
	// commitment update, plus some other meta-data...Or just use OP_RETURN
	// to help out?
	// currently going for: nSequence/nLockTime overloading
	channelDB *channeldb.DB

	// stateMtx protects concurrent access to the state struct.
	stateMtx     sync.RWMutex
	channelState channeldb.OpenChannel

	// TODO(roasbeef): create and embed 'Service' interface w/ below?
	started  int32
	shutdown int32

	quit chan struct{}
	wg   sync.WaitGroup
}

// newLightningChannel...
func newLightningChannel(wallet *LightningWallet, events *chainntnfs.ChainNotifier,
	chanDB *channeldb.DB, state channeldb.OpenChannel) (*LightningChannel, error) {

	return &LightningChannel{
		wallet:        wallet,
		channelEvents: events,
		channelDB:     chanDB,
		channelState:  state,
	}, nil
}

// AddHTLC...
func (lc *LightningChannel) AddHTLC() {
}

// SettleHTLC...
func (lc *LightningChannel) SettleHTLC() {
}

// OurBalance...
func (lc *LightningChannel) OurBalance() btcutil.Amount {
	return 0
}

// TheirBalance...
func (lc *LightningChannel) TheirBalance() btcutil.Amount {
	return 0
}

// CurrentCommitTx...
func (lc *LightningChannel) CurrentCommitTx() *btcutil.Tx {
	return nil
}

// SignTheirCommitTx...
func (lc *LightningChannel) SignTheirCommitTx(commitTx *btcutil.Tx) error {
	return nil
}

// AddTheirSig...
func (lc *LightningChannel) AddTheirSig(sig []byte) error {
	return nil
}

// VerifyCommitmentUpdate...
func (lc *LightningChannel) VerifyCommitmentUpdate() error {
	return nil
}

// createCommitTx...
func createCommitTx(fundingOutput *wire.TxIn, ourKey, theirKey *btcec.PublicKey,
	revokeHash [wire.HashSize]byte, csvTimeout uint32, channelAmt btcutil.Amount) (*wire.MsgTx, error) {

	// First, we create the script paying to us. This script is spendable
	// under two conditions: either the 'csvTimeout' has passed and we can
	// redeem our funds, or they have the pre-image to 'revokeHash'.
	scriptToUs := txscript.NewScriptBuilder()

	// If the pre-image for the revocation hash is presented, then allow a
	// spend provided the proper signature.
	scriptToUs.AddOp(txscript.OP_HASH160)
	scriptToUs.AddData(revokeHash[:])
	scriptToUs.AddOp(txscript.OP_EQUAL)
	scriptToUs.AddOp(txscript.OP_IF)
	scriptToUs.AddData(theirKey.SerializeCompressed())
	scriptToUs.AddOp(txscript.OP_ELSE)

	// Otherwise, we can re-claim our funds after a CSV delay of
	// 'csvTimeout' timeout blocks, and a valid signature.
	scriptToUs.AddInt64(int64(csvTimeout))
	scriptToUs.AddOp(txscript.OP_NOP3) // CSV
	scriptToUs.AddOp(txscript.OP_DROP)
	scriptToUs.AddData(ourKey.SerializeCompressed())
	scriptToUs.AddOp(txscript.OP_ENDIF)
	scriptToUs.AddOp(txscript.OP_CHECKSIG)

	// TODO(roasbeef): store
	ourRedeemScript, err := scriptToUs.Script()
	if err != nil {
		return nil, err
	}
	payToUsScriptHash, err := scriptHashPkScript(ourRedeemScript)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to them. This is just a regular
	// P2PKH-ike output. However, we instead use P2SH.
	scriptToThem := txscript.NewScriptBuilder()
	scriptToThem.AddOp(txscript.OP_DUP)
	scriptToThem.AddOp(txscript.OP_HASH160)
	scriptToThem.AddData(btcutil.Hash160(theirKey.SerializeCompressed()))
	scriptToThem.AddOp(txscript.OP_EQUALVERIFY)
	scriptToThem.AddOp(txscript.OP_CHECKSIG)

	theirRedeemScript, err := scriptToThem.Script()
	if err != nil {
		return nil, err
	}
	payToThemScriptHash, err := scriptHashPkScript(theirRedeemScript)
	if err != nil {
		return nil, err
	}

	// Now that both output scripts have been created, we can finally create
	// the transaction itself.
	commitTx := wire.NewMsgTx()
	commitTx.AddTxIn(fundingOutput)
	// TODO(roasbeef): we default to blocks, make configurable as part of
	// channel reservation.
	commitTx.TxIn[0].Sequence = lockTimeToSequence(false, csvTimeout)
	commitTx.AddTxOut(wire.NewTxOut(int64(channelAmt), payToUsScriptHash))
	commitTx.AddTxOut(wire.NewTxOut(int64(channelAmt), payToThemScriptHash))

	// Sort the transaction according to the agreed upon cannonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	txsort.InPlaceSort(commitTx)
	return commitTx, nil
}

// lockTimeToSequence converts the passed relative locktime to a sequence
// number in accordance to BIP-68.
// See: https://github.com/bitcoin/bips/blob/master/bip-0068.mediawiki
//  * (Compatibility)
func lockTimeToSequence(isSeconds bool, locktime uint32) uint32 {
	if !isSeconds {
		// The locktime is to be expressed in confirmations. Apply the
		// mask to restrict the number of confirmations to 65,535 or
		// 1.25 years.
		return SequenceLockTimeMask & locktime
	}

	// Set the 22nd bit which indicates the lock time is in seconds, then
	// shift the locktime over by 9 since the time granularity is in
	// 512-second intervals (2^9). This results in a max lock-time of
	// 33,554,431 seconds, or 1.06 years.
	return SequenceLockTimeSeconds | (locktime >> 9)
}

//TODO(j): Creates a CLTV-only funding Tx (reserve is *REQUIRED*)
//This works for only CLTV soft-fork (no CSV/segwit soft-fork in yet)
//
//Commit funds to Funding Tx, will timeout after the fundingTimeLock and refund
//back using CLTV. As there is no way to enforce HTLCs, we rely upon a reserve
//and have each party's HTLCs in-transit be less than their Commitment reserve.
//In the event that someone incorrectly broadcasts an old Commitment TX, then
//the counterparty claims the full reserve. It may be possible for either party
//to claim the HTLC(!!! But it's okay because the "honest" party is made whole
//via the reserve). If it's two-funder there are two outputs and the
//Commitments spends from both outputs in the Funding Tx. Two-funder requires
//the ourKey/theirKey sig positions to be swapped (should be in 1 funding tx).
//
//Quick note before I forget: The revocation hash is used in CLTV-only for
//single-funder (without an initial payment) *as part of an additional output
//in the Commitment Tx for the reserve*. This is to establish a unidirectional
//channel UNITL the recipient has sufficient funds. When the recipient has
//sufficient funds, the revocation is exchanged and allows the recipient to
//claim the full reserve as penalty if the incorrect Commitment is broadcast
//(otherwise it's timelocked refunded back to the sender). From then on, there
//is no additional output in Commitment Txes. [side caveat, first payment must
//be above minimum UTXO output size in single-funder] For now, let's keep it
//simple and assume dual funder (with both funding above reserve)
func createCLTVFundingTx(fundingTimeLock int64, ourKey *btcec.PublicKey, theirKey *btcec.PublicKey) (*wire.MsgTx, error) {
	script := txscript.NewScriptBuilder()
	//In the scriptSig on the top of the stack, there will be either a 0 or
	//1 pushed.
	//So the scriptSig will be either:
	//<BobSig> <AliceSig> <1>
	//<BobSig> <RevocationHash> <0>
	//(Alice and Bob can be swapped depending on who's funding)

	//If this is a 2-of-2 multisig, read the first sig
	script.AddOp(txscript.OP_IF)
	//Sig2 (not P2PKH, the pubkey is in the redeemScript)
	script.AddData(ourKey.SerializeCompressed())
	script.AddOp(txscript.OP_CHECKSIGVERIFY) //gotta be verify!

	//If this is timed out
	script.AddOp(txscript.OP_ELSE)
	script.AddInt64(fundingTimeLock)
	script.AddOp(txscript.OP_NOP2) //CLTV
	//Sig (not P2PKH, the pubkey is in the redeemScript)
	script.AddOp(txscript.OP_CHECKSIG)
	script.AddOp(txscript.OP_DROP)
	script.AddOp(txscript.OP_ENDIF)

	//Read the other sig if it's 2-of-2, only one if it's timed out
	script.AddData(theirKey.SerializeCompressed())
	script.AddOp(txscript.OP_CHECKSIG)

	fundingTx := wire.NewMsgTx()
	//TODO(j) Add the inputs/outputs

	return fundingTx, nil
}
