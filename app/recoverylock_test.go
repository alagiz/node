//go:build recoverylock_test

package app_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	coreaddress "cosmossdk.io/core/address"
	sdkmath "cosmossdk.io/math"
	"cosmossdk.io/x/feegrant"
	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client"
	clienttx "github.com/cosmos/cosmos-sdk/client/tx"
	"github.com/cosmos/cosmos-sdk/codec/address"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	vestingtypes "github.com/cosmos/cosmos-sdk/x/auth/vesting/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	crisistypes "github.com/cosmos/cosmos-sdk/x/crisis/types"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	govv1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"
	govv1beta1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	transfertypes "github.com/cosmos/ibc-go/v10/modules/apps/transfer/types"
	channelv1 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/types"
	channelv2 "github.com/cosmos/ibc-go/v10/modules/core/04-channel/v2/types"
	"github.com/stretchr/testify/require"

	audit "pkg.akt.dev/go/node/audit/v1"
	bme "pkg.akt.dev/go/node/bme/v1"
	cert "pkg.akt.dev/go/node/cert/v1"
	deployment "pkg.akt.dev/go/node/deployment/v1"
	deploymentmsg "pkg.akt.dev/go/node/deployment/v1beta4"
	escrowmodule "pkg.akt.dev/go/node/escrow/module"
	escrowtypes "pkg.akt.dev/go/node/escrow/types/v1"
	escrow "pkg.akt.dev/go/node/escrow/v1"
	market "pkg.akt.dev/go/node/market/v1"
	marketmsg "pkg.akt.dev/go/node/market/v1beta5"
	oracle "pkg.akt.dev/go/node/oracle/v2"
	provider "pkg.akt.dev/go/node/provider/v1beta4"
	sink "pkg.akt.dev/go/node/sink/v1"
	deposit "pkg.akt.dev/go/node/types/deposit/v1"
	wasmfilter "pkg.akt.dev/go/node/wasm/v1"
	"pkg.akt.dev/go/sdkutil"

	"pkg.akt.dev/node/v2/app"
	"pkg.akt.dev/node/v2/app/recoverylock"
)

const lockHeight = 20

// useLockTestConfig enables the lock with the test values in
// recoverylock/testdata for apps and configs built during the test.
func useLockTestConfig(t *testing.T) {
	t.Helper()
	listed, err := os.ReadFile("recoverylock/testdata/listed.txt")
	require.NoError(t, err)
	params, err := os.ReadFile("recoverylock/testdata/params.json")
	require.NoError(t, err)
	t.Cleanup(recoverylock.UseTestConfig(listed, params))
}

// TestRecoveryLockDisabledByDefault checks that the committed configuration
// leaves the app without a lock.
func TestRecoveryLockDisabledByDefault(t *testing.T) {
	a := app.Setup(app.WithHome(t.TempDir()), app.WithGenesis(app.GenesisStateWithValSet))
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	require.Nil(t, a.RecoveryLock)
}

func placeholderKey(seed string) *secp256k1.PrivKey {
	digest := sha256.Sum256([]byte(seed))
	return &secp256k1.PrivKey{Key: digest[:]}
}

func placeholderAddress(seed string) sdk.AccAddress {
	return sdk.AccAddress(placeholderKey(seed).PubKey().Address())
}

type withdrawStore struct {
	addr           sdk.AccAddress
	reads, deletes int
}

func (d *withdrawStore) GetDelegatorWithdrawAddr(_ context.Context, wallet sdk.AccAddress) (sdk.AccAddress, error) {
	d.reads++
	if d.addr == nil {
		return wallet, nil
	}
	return d.addr, nil
}

func (d *withdrawStore) DeleteDelegatorWithdrawAddr(_ context.Context, wallet, old sdk.AccAddress) error {
	d.deletes++
	d.addr = nil
	return nil
}

type lockFixture struct {
	lock                           *recoverylock.Lock
	txConfig                       client.TxConfig
	ctx                            sdk.Context
	withdraw                       *withdrawStore
	wallet, recovery, payer, other sdk.AccAddress
	ac                             coreaddress.Codec
}

func newLockFixture(t *testing.T) lockFixture {
	t.Helper()
	enc := sdkutil.MakeEncodingConfig()
	app.ModuleBasics().RegisterInterfaces(enc.InterfaceRegistry)
	ac := enc.SigningOptions.AddressCodec
	useLockTestConfig(t)
	cfg := recoverylock.MustConfig(ac, nil, nil)
	withdraw := &withdrawStore{}
	return lockFixture{
		lock:     recoverylock.New(cfg, enc.Codec, ac, withdraw, func() int64 { return lockHeight - 1 }),
		txConfig: enc.TxConfig,
		ctx:      sdk.Context{}.WithContext(context.Background()).WithBlockHeight(lockHeight).WithBlockTime(time.Unix(1_750_000_000, 0)),
		withdraw: withdraw,
		wallet:   placeholderAddress("recoverylock-test-listed-0"),
		recovery: placeholderAddress("recoverylock-test-recovery"),
		payer:    placeholderAddress("recoverylock-test-fee-payer-0"),
		other:    placeholderAddress("recoverylock-unlisted"),
		ac:       ac,
	}
}

func (f lockFixture) acc(t *testing.T, a sdk.AccAddress) string {
	t.Helper()
	s, err := f.ac.BytesToString(a)
	require.NoError(t, err)
	return s
}

func lockCoins(amount int64) sdk.Coins {
	return sdk.NewCoins(sdk.NewInt64Coin(sdkutil.DenomUakt, amount))
}

func (f lockFixture) tx(t *testing.T, payer, granter sdk.AccAddress, amt int64, msgs ...sdk.Msg) sdk.Tx {
	t.Helper()
	b := f.txConfig.NewTxBuilder()
	require.NoError(t, b.SetMsgs(msgs...))
	if payer != nil {
		b.SetFeePayer(payer)
	}
	if granter != nil {
		b.SetFeeGranter(granter)
	}
	b.SetFeeAmount(lockCoins(amt))
	return b.GetTx()
}

func (f lockFixture) ante(t *testing.T, ctx sdk.Context, tx sdk.Tx) error {
	t.Helper()
	called := false
	_, err := (recoverylock.Decorator{L: f.lock}).AnteHandle(ctx, tx, false,
		func(ctx sdk.Context, _ sdk.Tx, _ bool) (sdk.Context, error) {
			called = true
			return ctx, nil
		})
	require.Equal(t, err == nil, called)
	return err
}

func TestRecoveryLockBlockedPaths(t *testing.T) {
	f := newLockFixture(t)
	w, o, r := f.acc(t, f.wallet), f.acc(t, f.other), f.acc(t, f.recovery)
	validator, err := address.NewBech32Codec("akashvaloper").BytesToString(f.wallet)
	require.NoError(t, err)
	deployID := deployment.DeploymentID{Owner: w}
	groupID := deployment.GroupID{Owner: w}
	bidID := market.BidID{Owner: w, Provider: w}
	leaseID := market.LeaseID{Owner: w, Provider: w}
	// These are signer fixtures. The lock must deny them before their handlers
	// validate module-specific fields or look up live state.
	msgs := []sdk.Msg{
		&banktypes.MsgSend{FromAddress: w, ToAddress: o},
		&banktypes.MsgMultiSend{Inputs: []banktypes.Input{{Address: w}}, Outputs: []banktypes.Output{{Address: r}, {Address: o}}},
		&banktypes.MsgUpdateParams{Authority: w},
		&banktypes.MsgSetSendEnabled{Authority: w},
		&stakingtypes.MsgCreateValidator{ValidatorAddress: validator},
		&stakingtypes.MsgEditValidator{ValidatorAddress: validator},
		&stakingtypes.MsgDelegate{DelegatorAddress: w},
		&stakingtypes.MsgBeginRedelegate{DelegatorAddress: w},
		&stakingtypes.MsgCancelUnbondingDelegation{DelegatorAddress: w},
		&stakingtypes.MsgUpdateParams{Authority: w},
		&slashingtypes.MsgUnjail{ValidatorAddr: validator},
		&distrtypes.MsgSetWithdrawAddress{DelegatorAddress: w, WithdrawAddress: o},
		&distrtypes.MsgFundCommunityPool{Depositor: w},
		&distrtypes.MsgDepositValidatorRewardsPool{Depositor: w},
		&distrtypes.MsgCommunityPoolSpend{Authority: w},
		&distrtypes.MsgUpdateParams{Authority: w},
		&govv1.MsgSubmitProposal{Proposer: w},
		&govv1beta1.MsgSubmitProposal{Proposer: w},
		&govv1.MsgDeposit{Depositor: w},
		&govv1beta1.MsgDeposit{Depositor: w},
		&govv1.MsgCancelProposal{Proposer: w},
		&govv1.MsgExecLegacyContent{Authority: w},
		&govv1.MsgVote{Voter: w},
		&govv1.MsgVoteWeighted{Voter: w},
		&govv1beta1.MsgVote{Voter: w},
		&govv1beta1.MsgVoteWeighted{Voter: w},
		&govv1.MsgUpdateParams{Authority: w},
		&vestingtypes.MsgCreateVestingAccount{FromAddress: w},
		&vestingtypes.MsgCreatePermanentLockedAccount{FromAddress: w},
		&vestingtypes.MsgCreatePeriodicVestingAccount{FromAddress: w},
		&crisistypes.MsgVerifyInvariant{Sender: w},
		&authz.MsgGrant{Granter: w, Grantee: o},
		&authz.MsgRevoke{Granter: w, Grantee: o},
		&authz.MsgExec{Grantee: w},
		&feegrant.MsgGrantAllowance{Granter: w, Grantee: o},
		&feegrant.MsgRevokeAllowance{Granter: w, Grantee: o},
		&feegrant.MsgPruneAllowances{Pruner: w},
		&transfertypes.MsgTransfer{Sender: w, Token: lockCoins(1)[0]},
		&channelv1.MsgRecvPacket{Signer: w},
		&channelv1.MsgAcknowledgement{Signer: w},
		&channelv1.MsgTimeout{Signer: w},
		&channelv1.MsgTimeoutOnClose{Signer: w},
		&channelv2.MsgSendPacket{Signer: w},
		&channelv2.MsgRecvPacket{Signer: w},
		&channelv2.MsgAcknowledgement{Signer: w},
		&channelv2.MsgTimeout{Signer: w},
		&wasmtypes.MsgStoreCode{Sender: w},
		&wasmtypes.MsgInstantiateContract{Sender: w},
		&wasmtypes.MsgInstantiateContract2{Sender: w},
		&wasmtypes.MsgMigrateContract{Sender: w},
		&wasmtypes.MsgStoreAndInstantiateContract{Authority: w},
		&wasmtypes.MsgStoreAndMigrateContract{Authority: w},
		&wasmtypes.MsgUpdateAdmin{Sender: w},
		&wasmtypes.MsgClearAdmin{Sender: w},
		&wasmtypes.MsgUpdateContractLabel{Sender: w},
		&wasmtypes.MsgSudoContract{Authority: w},
		&wasmtypes.MsgPinCodes{Authority: w},
		&wasmtypes.MsgUnpinCodes{Authority: w},
		&wasmtypes.MsgUpdateInstantiateConfig{Sender: w},
		&wasmtypes.MsgAddCodeUploadParamsAddresses{Authority: w},
		&wasmtypes.MsgRemoveCodeUploadParamsAddresses{Authority: w},
		&wasmtypes.MsgUpdateParams{Authority: w},
		&deploymentmsg.MsgCreateDeployment{ID: deployID},
		&deploymentmsg.MsgUpdateDeployment{ID: deployID},
		&deploymentmsg.MsgCloseGroup{ID: groupID},
		&deploymentmsg.MsgPauseGroup{ID: groupID},
		&deploymentmsg.MsgStartGroup{ID: groupID},
		&deploymentmsg.MsgUpdateParams{Authority: w},
		&escrow.MsgAccountDeposit{Signer: w},
		&marketmsg.MsgCreateBid{ID: bidID},
		&marketmsg.MsgCloseBid{ID: bidID},
		&marketmsg.MsgCreateLease{BidID: bidID},
		&marketmsg.MsgWithdrawLease{ID: leaseID},
		&marketmsg.MsgCloseLease{ID: leaseID},
		&marketmsg.MsgLeaseStartReclaim{ID: leaseID},
		&marketmsg.MsgUpdateParams{Authority: w},
		&provider.MsgCreateProvider{Owner: w},
		&provider.MsgUpdateProvider{Owner: w},
		&provider.MsgDeleteProvider{Owner: w},
		&cert.MsgCreateCertificate{Owner: w},
		&cert.MsgRevokeCertificate{ID: cert.ID{Owner: w}},
		&audit.MsgSignProviderAttributes{Owner: o, Auditor: w},
		&audit.MsgDeleteProviderAttributes{Owner: o, Auditor: w},
		&bme.MsgBurnMint{Owner: w},
		&bme.MsgMintACT{Owner: w},
		&bme.MsgFundVault{Authority: w},
		&bme.MsgUpdateParams{Authority: w},
		&sink.MsgSink{Sender: w},
		&sink.MsgRequestVoid{Authority: w},
		&sink.MsgReportOutcome{Reporter: w},
		&sink.MsgVetoVoid{Signer: w},
		&sink.MsgRestartTransfer{Authority: w},
		&sink.MsgConfirmMint{Authority: w},
		&sink.MsgConfirmVoid{Authority: w},
		&sink.MsgFinalizeRefund{Executor: w},
		&sink.MsgGuardianPause{Guardian: w},
		&sink.MsgUpdateParams{Authority: w},
		&oracle.MsgAddPriceEntry{Signer: w},
		&oracle.MsgUpdateParams{Authority: w},
		&wasmfilter.MsgUpdateParams{Authority: w},
	}
	for _, msg := range msgs {
		t.Run(sdk.MsgTypeURL(msg), func(t *testing.T) {
			tx := f.tx(t, f.payer, nil, 1, msg)
			require.ErrorIs(t, f.ante(t, f.ctx, tx), recoverylock.ErrLocked)
		})
	}
}

func TestRecoveryLockAllowedPaths(t *testing.T) {
	f := newLockFixture(t)
	w, r, o := f.acc(t, f.wallet), f.acc(t, f.recovery), f.acc(t, f.other)
	validator, err := address.NewBech32Codec("akashvaloper").BytesToString(f.wallet)
	require.NoError(t, err)
	allowed := []sdk.Msg{
		&banktypes.MsgSend{FromAddress: w, ToAddress: r, Amount: lockCoins(2)},
		&banktypes.MsgMultiSend{Inputs: []banktypes.Input{{Address: w, Coins: lockCoins(2)}}, Outputs: []banktypes.Output{{Address: r, Coins: lockCoins(2)}}},
		&distrtypes.MsgSetWithdrawAddress{DelegatorAddress: w, WithdrawAddress: w},
		&distrtypes.MsgSetWithdrawAddress{DelegatorAddress: w, WithdrawAddress: r},
		&deploymentmsg.MsgCloseDeployment{ID: deployment.DeploymentID{Owner: w, DSeq: 1}},
	}
	for _, msg := range allowed {
		require.NoError(t, f.ante(t, f.ctx, f.tx(t, f.payer, nil, 1, msg)), "%T", msg)
	}
	for _, msg := range []sdk.Msg{
		&stakingtypes.MsgUndelegate{DelegatorAddress: w, ValidatorAddress: validator, Amount: lockCoins(2)[0]},
		&distrtypes.MsgWithdrawDelegatorReward{DelegatorAddress: w, ValidatorAddress: validator},
		&distrtypes.MsgWithdrawValidatorCommission{ValidatorAddress: validator},
	} {
		tx := f.tx(t, f.payer, nil, 1, msg)
		for _, dest := range []sdk.AccAddress{f.wallet, f.recovery} {
			f.withdraw.addr = dest
			require.NoError(t, f.ante(t, f.ctx, tx), "%T", msg)
		}
		f.withdraw.addr = f.other
		require.ErrorIs(t, f.ante(t, f.ctx, tx), recoverylock.ErrLocked, "%T", msg)
	}
	for _, body := range []string{
		fmt.Sprintf(`{"transfer":{"recipient":%q,"amount":"1"}}`, r),
		fmt.Sprintf(`{"transfer_nft":{"recipient":%q,"token_id":"1"}}`, r),
		fmt.Sprintf(`{"decrease_allowance":{"spender":%q,"amount":"1"}}`, o),
		fmt.Sprintf(`{"revoke":{"spender":%q,"token_id":"1"}}`, o),
		fmt.Sprintf(`{"revoke_all":{"operator":%q}}`, o),
	} {
		msg := &wasmtypes.MsgExecuteContract{Sender: w, Contract: o, Msg: []byte(body)}
		require.NoError(t, f.ante(t, f.ctx, f.tx(t, f.payer, nil, 1, msg)))
		msg.Funds = lockCoins(1)
		require.ErrorIs(t, f.ante(t, f.ctx, f.tx(t, f.payer, nil, 1, msg)), recoverylock.ErrLocked)
	}
	for _, body := range []string{
		fmt.Sprintf(`{"transfer":{"recipient":%q,"amount":"1"}}`, o),
		fmt.Sprintf(`{"transfer_nft":{"recipient":%q,"token_id":"1"}}`, o),
		fmt.Sprintf(`{"transfer":{"recipient":%q,"recipient":%q,"amount":"1"}}`, o, r),
		fmt.Sprintf(`{"transfer":{"recipient":%q,"amount":1}}`, r),
		fmt.Sprintf(`{"decrease_allowance":{"spender":%q,"amount":"1","expires":{}}}`, o),
		`{"increase_allowance":{}}`, `{"approve":{}}`, `{"send":{}}`, `{"send_nft":{}}`,
		`{"revoke_all":{}} {}`,
	} {
		msg := &wasmtypes.MsgExecuteContract{Sender: w, Contract: o, Msg: []byte(body)}
		require.ErrorIs(t, f.ante(t, f.ctx, f.tx(t, f.payer, nil, 1, msg)), recoverylock.ErrLocked)
	}
}

func TestRecoveryLockBurnACTDestinations(t *testing.T) {
	f := newLockFixture(t)
	w, r, o := f.acc(t, f.wallet), f.acc(t, f.recovery), f.acc(t, f.other)
	msg := &bme.MsgBurnACT{Owner: w, CoinsToBurn: sdk.NewInt64Coin(sdkutil.DenomUact, 1_000_000)}
	msg.To = w
	require.NoError(t, f.ante(t, f.ctx, f.tx(t, f.payer, nil, 1, msg)))
	for _, to := range []string{r, o} {
		msg.To = to
		require.ErrorContains(t, f.ante(t, f.ctx, f.tx(t, f.payer, nil, 1, msg)), "ACT burn must pay the signer")
	}
	msg.To = w
	require.ErrorIs(t, f.ante(t, f.ctx, f.tx(t, f.other, nil, 1, msg)), recoverylock.ErrLocked)
	require.ErrorIs(t, f.ante(t, f.ctx, f.tx(t, f.payer, nil, 0, msg)), recoverylock.ErrLocked)
}

type testRouter struct{}

func (testRouter) Handler(sdk.Msg) baseapp.MsgServiceHandler {
	return func(sdk.Context, sdk.Msg) (*sdk.Result, error) { return &sdk.Result{}, nil }
}

func (r testRouter) HandlerByTypeURL(string) baseapp.MsgServiceHandler { return r.Handler(nil) }

func TestRecoveryLockFeesWindowAndDispatch(t *testing.T) {
	f := newLockFixture(t)
	w, r, o := f.acc(t, f.wallet), f.acc(t, f.recovery), f.acc(t, f.other)
	bad := &banktypes.MsgSend{FromAddress: w, ToAddress: o, Amount: lockCoins(2)}
	good := &banktypes.MsgSend{FromAddress: w, ToAddress: r, Amount: lockCoins(2)}
	ordinary := &banktypes.MsgSend{FromAddress: o, ToAddress: w, Amount: lockCoins(2)}
	for _, tx := range []sdk.Tx{
		f.tx(t, nil, nil, 1, good),
		f.tx(t, f.other, nil, 1, good),
		f.tx(t, f.payer, nil, 0, good),
		f.tx(t, f.payer, f.other, 1, good),
		f.tx(t, f.other, f.wallet, 1, ordinary),
		f.tx(t, f.wallet, nil, 1, ordinary),
		f.tx(t, f.payer, nil, 1, ordinary, bad),
	} {
		require.ErrorIs(t, f.ante(t, f.ctx, tx), recoverylock.ErrLocked)
	}
	require.NoError(t, f.ante(t, f.ctx, f.tx(t, nil, f.other, 1, ordinary)))
	tx := f.tx(t, nil, nil, 1, bad)
	before := f.ctx.WithBlockHeight(lockHeight - 1)
	require.NoError(t, f.ante(t, before, tx))
	require.ErrorIs(t, f.ante(t, f.ctx, tx), recoverylock.ErrLocked)
	require.NoError(t, f.ante(t, f.ctx.WithBlockTime(time.Unix(4102444800, 0)), tx))
	require.ErrorIs(t, f.ante(t, before.WithIsCheckTx(true), tx), recoverylock.ErrLocked)
	restart := f.ctx.WithBlockHeight(0).WithBlockTime(time.Time{}).WithIsCheckTx(true)
	require.ErrorIs(t, f.ante(t, restart, tx), recoverylock.ErrLocked)

	wrap := func(msg sdk.Msg) sdk.Msg {
		exec := authz.NewMsgExec(f.other, []sdk.Msg{msg})
		return &exec
	}
	nestedGood, nestedBad := wrap(wrap(good)), wrap(wrap(bad))
	require.NoError(t, f.ante(t, f.ctx, f.tx(t, f.payer, nil, 1, nestedGood)))
	require.ErrorIs(t, f.ante(t, f.ctx, f.tx(t, nil, nil, 1, nestedGood)), recoverylock.ErrLocked)
	require.ErrorIs(t, f.ante(t, f.ctx, f.tx(t, f.payer, nil, 1, nestedBad)), recoverylock.ErrLocked)
	router := recoverylock.Router{Inner: testRouter{}, L: f.lock}
	_, err := router.Handler(nestedGood)(f.ctx, nestedGood)
	require.ErrorIs(t, err, recoverylock.ErrLocked)
	_, err = router.HandlerByTypeURL(sdk.MsgTypeURL(good))(f.ctx, good)
	require.ErrorIs(t, err, recoverylock.ErrLocked)
	_, err = router.Handler(ordinary)(f.ctx, ordinary)
	require.NoError(t, err)
	_, err = router.Handler(good)(before, good)
	require.NoError(t, err)
	unlistedNested := sdk.Msg(ordinary)
	for i := 0; i < recoverylock.MaxDepth; i++ {
		unlistedNested = wrap(unlistedNested)
	}
	require.NoError(t, f.ante(t, f.ctx, f.tx(t, nil, nil, 1, unlistedNested)))
	require.ErrorIs(t, f.ante(t, f.ctx, f.tx(t, nil, nil, 1, wrap(unlistedNested))), recoverylock.ErrLocked)
}

func TestRecoveryLockStakingHooks(t *testing.T) {
	f := newLockFixture(t)
	hooks := recoverylock.StakingHooks{L: f.lock}
	f.withdraw.addr = f.other
	require.NoError(t, hooks.BeforeDelegationSharesModified(f.ctx.WithBlockHeight(lockHeight-1), f.wallet, nil))
	require.NoError(t, hooks.BeforeDelegationRemoved(f.ctx, f.other, nil))
	require.Zero(t, f.withdraw.reads)
	require.NoError(t, hooks.BeforeDelegationSharesModified(f.ctx, f.wallet, nil))
	require.Equal(t, 1, f.withdraw.deletes)
	f.withdraw.addr = f.recovery
	require.NoError(t, hooks.BeforeDelegationRemoved(f.ctx, f.wallet, nil))
	require.Equal(t, 1, f.withdraw.deletes)
	f.withdraw.addr = f.other
	require.NoError(t, hooks.AfterValidatorRemoved(f.ctx, nil, sdk.ValAddress(f.wallet)))
	require.Equal(t, 2, f.withdraw.deletes)
}

func TestRecoveryLockAppWiring(t *testing.T) {
	useLockTestConfig(t)
	a := app.Setup(app.WithHome(t.TempDir()), app.WithGenesis(app.GenesisStateWithValSet))
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	require.NotNil(t, a.RecoveryLock)
	ctx := a.NewContext(false).WithBlockHeight(lockHeight).WithBlockTime(time.Unix(1_750_000_000, 0))
	walletKey := placeholderKey("recoverylock-test-listed-0")
	payerKey := placeholderKey("recoverylock-test-fee-payer-0")
	wallet, payer := sdk.AccAddress(walletKey.PubKey().Address()), sdk.AccAddress(payerKey.PubKey().Address())
	recovery := placeholderAddress("recoverylock-test-recovery")
	other := placeholderAddress("recoverylock-unlisted")

	bank := a.Keepers.Cosmos.Bank
	acct := a.Keepers.Cosmos.Acct
	for _, addr := range []sdk.AccAddress{wallet, payer} {
		acct.SetAccount(ctx, acct.NewAccountWithAddress(ctx, addr))
		require.NoError(t, bank.MintCoins(ctx, minttypes.ModuleName, lockCoins(1000)))
		require.NoError(t, bank.SendCoinsFromModuleToAccount(ctx, minttypes.ModuleName, addr, lockCoins(1000)))
	}

	buildTx := func(dest sdk.AccAddress, keys ...*secp256k1.PrivKey) sdk.Tx {
		msg := &banktypes.MsgSend{FromAddress: wallet.String(), ToAddress: dest.String(), Amount: lockCoins(2)}
		return signedLockTestTx(t, a, ctx, payer, msg, keys...)
	}

	ante := a.AnteHandler()
	payerStart := bank.GetBalance(ctx, payer, sdkutil.DenomUakt)
	_, err := ante(ctx, buildTx(other, walletKey, payerKey), false)
	require.ErrorIs(t, err, recoverylock.ErrLocked)
	require.Equal(t, payerStart, bank.GetBalance(ctx, payer, sdkutil.DenomUakt))
	cache, _ := ctx.CacheContext()
	_, err = ante(cache, buildTx(recovery, walletKey), false)
	require.Error(t, err, "the registered payer must sign")
	acceptedCtx, err := ante(ctx, buildTx(recovery, walletKey, payerKey), false)
	require.NoError(t, err)
	require.Equal(t, payerStart.Sub(lockCoins(3)[0]), bank.GetBalance(ctx, payer, sdkutil.DenomUakt))

	walletStart := bank.GetBalance(ctx, wallet, sdkutil.DenomUakt)
	vault := &bme.MsgFundVault{
		Authority: authtypes.NewModuleAddress(govtypes.ModuleName).String(),
		Source:    wallet.String(), Amount: lockCoins(1)[0],
	}
	vaultHandler := a.MsgServiceRouter().Handler(vault)
	require.NotNil(t, vaultHandler)
	_, err = vaultHandler(acceptedCtx, vault)
	require.ErrorIs(t, err, recoverylock.ErrLocked)
	require.Equal(t, walletStart, bank.GetBalance(ctx, wallet, sdkutil.DenomUakt))
	require.ErrorIs(t, bank.SendCoinsFromAccountToModule(acceptedCtx, wallet, authtypes.FeeCollectorName, lockCoins(1)), recoverylock.ErrLocked)
	msg := &banktypes.MsgSend{FromAddress: wallet.String(), ToAddress: recovery.String(), Amount: lockCoins(2)}
	handler := a.MsgServiceRouter().Handler(msg)
	require.NotNil(t, handler)
	_, err = handler(acceptedCtx, msg)
	require.NoError(t, err)
	require.Equal(t, walletStart.Sub(lockCoins(2)[0]), bank.GetBalance(ctx, wallet, sdkutil.DenomUakt))
	require.Equal(t, lockCoins(2)[0], bank.GetBalance(ctx, recovery, sdkutil.DenomUakt))

	authzKeeper := a.Keepers.Cosmos.Authz
	expiry := ctx.BlockTime().Add(time.Hour)
	require.NoError(t, authzKeeper.SaveGrant(ctx, other, wallet, authz.NewGenericAuthorization(sdk.MsgTypeURL(msg)), &expiry))
	_, err = authzKeeper.DispatchActions(ctx, other, []sdk.Msg{msg})
	require.ErrorIs(t, err, recoverylock.ErrLocked)

	for _, absent := range []string{
		"/ibc.applications.interchain_accounts.controller.v1.MsgSendTx",
		"/ibc.applications.interchain_accounts.host.v1.MsgUpdateParams",
		"/cosmos.staking.v1beta1.MsgTokenizeShares",
		"/cosmos.staking.v1beta1.MsgRedeemTokensforShares",
		"/akash.take.v1.MsgUpdateParams",
		"/akash.staking.v1beta3.MsgUpdateParams",
	} {
		require.Nil(t, a.MsgServiceRouter().HandlerByTypeURL(absent))
	}
}

// TestRecoveryLockAppIBCV2SendPacket covers a raw IBC v2 MsgSendPacket signed
// by an unlisted account with a transfer payload that names a listed wallet as
// sender. The lock admits the transaction (no listed signer), the transfer
// callback that MsgSendPacket runs refuses the payload (sender must equal the
// signer), and the escrow debit from the listed wallet is refused by the coin
// rule if it were reached.
func TestRecoveryLockAppIBCV2SendPacket(t *testing.T) {
	useLockTestConfig(t)
	a := app.Setup(app.WithHome(t.TempDir()), app.WithGenesis(app.GenesisStateWithValSet))
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	require.NotNil(t, a.RecoveryLock)
	ctx := a.NewContext(false).WithBlockHeight(lockHeight).WithBlockTime(time.Unix(1_750_000_000, 0))
	wallet := placeholderAddress("recoverylock-test-listed-0")
	otherKey := placeholderKey("recoverylock-unlisted")
	other := sdk.AccAddress(otherKey.PubKey().Address())
	acct, bank := a.Keepers.Cosmos.Acct, a.Keepers.Cosmos.Bank
	for _, addr := range []sdk.AccAddress{wallet, other} {
		acct.SetAccount(ctx, acct.NewAccountWithAddress(ctx, addr))
		require.NoError(t, bank.MintCoins(ctx, minttypes.ModuleName, lockCoins(1000)))
		require.NoError(t, bank.SendCoinsFromModuleToAccount(ctx, minttypes.ModuleName, addr, lockCoins(1000)))
	}
	walletStart := bank.GetAllBalances(ctx, wallet)

	data := transfertypes.NewFungibleTokenPacketData(sdkutil.DenomUakt, "10", wallet.String(), "cosmos1receiver", "")
	value, err := transfertypes.MarshalPacketData(data, transfertypes.V1, transfertypes.EncodingProtobuf)
	require.NoError(t, err)
	payload := channelv2.NewPayload(transfertypes.PortID, transfertypes.PortID, transfertypes.V1, transfertypes.EncodingProtobuf, value)
	msg := channelv2.NewMsgSendPacket("07-tendermint-0", uint64(ctx.BlockTime().Unix()+600), other.String(), payload)

	// Not a listed signer and not a configured fee payer: the lock admits it.
	_, err = a.AnteHandler()(ctx, signedLockTestTx(t, a, ctx, other, msg, otherKey), false)
	require.NoError(t, err)

	cbs := a.Keepers.Cosmos.IBC.ChannelKeeperV2.Router.Route(transfertypes.PortID)
	require.NotNil(t, cbs)
	err = cbs.OnSendPacket(ctx, "07-tendermint-0", "07-tendermint-1", 1, payload, other)
	require.ErrorContains(t, err, "is different from signer")
	token := transfertypes.Token{Denom: transfertypes.NewDenom(sdkutil.DenomUakt), Amount: "10"}
	err = a.Keepers.Cosmos.Transfer.SendTransfer(ctx, transfertypes.PortID, "07-tendermint-0", token, wallet)
	require.ErrorIs(t, err, recoverylock.ErrLocked)
	require.Equal(t, walletStart, bank.GetAllBalances(ctx, wallet))
}

func TestRecoveryLockAppRecoveryExits(t *testing.T) {
	useLockTestConfig(t)
	a := app.Setup(app.WithHome(t.TempDir()), app.WithGenesis(app.GenesisStateWithValSet))
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	require.NotNil(t, a.RecoveryLock)
	ctx := a.NewContext(false).WithBlockHeight(lockHeight).WithBlockTime(time.Unix(1_750_000_000, 0))
	walletKey := placeholderKey("recoverylock-test-listed-0")
	payerKey := placeholderKey("recoverylock-test-fee-payer-0")
	otherKey := placeholderKey("recoverylock-unlisted")
	wallet := sdk.AccAddress(walletKey.PubKey().Address())
	payer := sdk.AccAddress(payerKey.PubKey().Address())
	other := sdk.AccAddress(otherKey.PubKey().Address())
	recovery := placeholderAddress("recoverylock-test-recovery")
	acct, bank := a.Keepers.Cosmos.Acct, a.Keepers.Cosmos.Bank
	for _, addr := range []sdk.AccAddress{wallet, payer, other} {
		acct.SetAccount(ctx, acct.NewAccountWithAddress(ctx, addr))
		require.NoError(t, bank.MintCoins(ctx, minttypes.ModuleName, lockCoins(1000)))
		require.NoError(t, bank.SendCoinsFromModuleToAccount(ctx, minttypes.ModuleName, addr, lockCoins(1000)))
	}

	t.Run("burn ACT", func(t *testing.T) {
		ctx, _ := ctx.CacheContext()
		burn := sdk.NewInt64Coin(sdkutil.DenomUact, 1_000_000)
		act := sdk.NewCoins(burn)
		require.NoError(t, bank.MintCoins(ctx, minttypes.ModuleName, act))
		require.NoError(t, bank.SendCoinsFromModuleToAccount(ctx, minttypes.ModuleName, wallet, act))
		id := oracle.DataID{Denom: sdkutil.DenomAkt, BaseDenom: sdkutil.DenomUSD}
		price := sdkmath.LegacyOneDec()
		require.NoError(t, a.Keepers.Akash.Oracle.SetAggregatedPrice(ctx, id, oracle.AggregatedPrice{
			Denom: id.Denom, TWAP: price, MedianPrice: price, MinPrice: price, MaxPrice: price, Timestamp: ctx.BlockTime(),
		}))
		require.NoError(t, a.Keepers.Akash.Oracle.SetPriceHealth(ctx, id, oracle.PriceHealth{Denom: id.Denom, IsHealthy: true}))

		// Direct vault funding is still blocked for this exact coin and destination.
		require.ErrorIs(t, bank.SendCoinsFromAccountToModule(ctx, wallet, bme.ModuleName, act), recoverylock.ErrLocked)
		msg := &bme.MsgBurnACT{Owner: wallet.String(), To: other.String(), CoinsToBurn: burn}
		tx := signedLockTestTx(t, a, ctx, payer, msg, walletKey, payerKey)
		_, err := a.AnteHandler()(ctx, tx, false)
		require.ErrorIs(t, err, recoverylock.ErrLocked)

		msg.To = recovery.String()
		require.ErrorIs(t, msg.ValidateBasic(), bme.ErrUnauthorized, "the pinned BME module requires self payout")
		msg.To = wallet.String()
		start := bank.GetBalance(ctx, wallet, sdkutil.DenomUakt)
		handler := a.MsgServiceRouter().Handler(msg)
		require.NotNil(t, handler)
		tx = signedLockTestTx(t, a, ctx, payer, msg, walletKey, payerKey)
		execCtx, err := a.AnteHandler()(ctx, tx, false)
		require.NoError(t, err)
		_, err = handler(execCtx, msg)
		require.NoError(t, err)
		require.True(t, bank.GetBalance(ctx, wallet, sdkutil.DenomUact).IsZero())
		require.Equal(t, burn, bank.GetBalance(ctx, acct.GetModuleAddress(bme.ModuleName), sdkutil.DenomUact))
		require.NoError(t, a.Keepers.Akash.Bme.EndBlocker(ctx))
		require.Equal(t, start.Add(sdk.NewCoin(sdkutil.DenomUakt, burn.Amount)), bank.GetBalance(ctx, wallet, sdkutil.DenomUakt))

		// The exception exists only in a transaction the Decorator admitted and
		// that consists of MsgBurnACT messages only.
		require.NoError(t, bank.MintCoins(ctx, minttypes.ModuleName, act))
		require.NoError(t, bank.SendCoinsFromModuleToAccount(ctx, minttypes.ModuleName, wallet, act))
		_, err = handler(ctx, msg)
		require.ErrorIs(t, err, recoverylock.ErrLocked, "no ante marker")
		send := &banktypes.MsgSend{FromAddress: wallet.String(), ToAddress: recovery.String(), Amount: lockCoins(1)}
		mixed := signedLockTestTxMsgs(t, a, ctx, payer, []sdk.Msg{msg, send}, walletKey, payerKey)
		mixedCtx, err := a.AnteHandler()(ctx, mixed, false)
		require.NoError(t, err)
		_, err = handler(mixedCtx, msg)
		require.ErrorIs(t, err, recoverylock.ErrLocked, "mixed transaction")
		require.Equal(t, burn, bank.GetBalance(ctx, wallet, sdkutil.DenomUact))

		// Before activation the listed wallet burns with its own fee as usual.
		pre := ctx.WithBlockHeight(lockHeight - 1)
		tx = signedLockTestTx(t, a, pre, wallet, msg, walletKey)
		preCtx, err := a.AnteHandler()(pre, tx, false)
		require.NoError(t, err)
		_, err = handler(preCtx, msg)
		require.NoError(t, err)
		require.True(t, bank.GetBalance(ctx, wallet, sdkutil.DenomUact).IsZero())

		// An unlisted owner burns with its own fee after activation as usual,
		// alone or together with other messages.
		require.NoError(t, bank.MintCoins(ctx, minttypes.ModuleName, act))
		require.NoError(t, bank.SendCoinsFromModuleToAccount(ctx, minttypes.ModuleName, other, act))
		otherBurn := &bme.MsgBurnACT{Owner: other.String(), To: other.String(), CoinsToBurn: sdk.NewInt64Coin(sdkutil.DenomUact, 400_000)}
		otherSend := &banktypes.MsgSend{FromAddress: other.String(), ToAddress: wallet.String(), Amount: lockCoins(1)}
		otherStart := bank.GetBalance(ctx, other, sdkutil.DenomUakt)
		tx = signedLockTestTxMsgs(t, a, ctx, other, []sdk.Msg{otherBurn, otherSend}, otherKey)
		otherCtx, err := a.AnteHandler()(ctx, tx, false)
		require.NoError(t, err)
		_, err = handler(otherCtx, otherBurn)
		require.NoError(t, err)
		_, err = handler(ctx, otherBurn)
		require.NoError(t, err, "unlisted burn without ante context")
		require.Equal(t, sdk.NewInt64Coin(sdkutil.DenomUact, 200_000), bank.GetBalance(ctx, other, sdkutil.DenomUact))
		require.Equal(t, otherStart.Sub(lockCoins(3)[0]), bank.GetBalance(ctx, other, sdkutil.DenomUakt))
	})

	t.Run("close deployment", func(t *testing.T) {
		ctx, _ := ctx.CacheContext()
		deployKeeper, escrowKeeper := a.Keepers.Akash.Deployment, a.Keepers.Akash.Escrow
		id := deployment.DeploymentID{Owner: wallet.String(), DSeq: 1}
		beforeLock := ctx.WithBlockHeight(lockHeight - 1)
		require.NoError(t, deployKeeper.Create(beforeLock, deployment.Deployment{ID: id, State: deployment.DeploymentActive}, nil))
		require.NoError(t, escrowKeeper.AccountCreate(beforeLock, id.ToEscrowAccountID(), wallet, []escrowtypes.Depositor{
			{Owner: wallet.String(), Source: deposit.SourceBalance, Height: beforeLock.BlockHeight(), Balance: sdk.NewDecCoinFromCoin(lockCoins(100)[0])},
			{Owner: other.String(), Source: deposit.SourceGrant, Height: beforeLock.BlockHeight(), Balance: sdk.NewDecCoinFromCoin(lockCoins(200)[0])},
		}))

		msg := &deploymentmsg.MsgCloseDeployment{ID: id}
		ante := a.AnteHandler()
		cache, _ := ctx.CacheContext()
		_, err := ante(cache, signedLockTestTx(t, a, cache, payer, msg, otherKey, payerKey), false)
		require.ErrorContains(t, err, "pubKey does not match signer address")

		handler := a.MsgServiceRouter().Handler(msg)
		require.NotNil(t, handler)
		execCtx, err := ante(ctx, signedLockTestTx(t, a, ctx, payer, msg, walletKey, payerKey), false)
		require.NoError(t, err)
		_, err = handler(execCtx, msg)
		require.NoError(t, err)

		closed, found := deployKeeper.GetDeployment(ctx, id)
		require.True(t, found)
		require.Equal(t, deployment.DeploymentClosed, closed.State)
		for _, addr := range []sdk.AccAddress{wallet, other} {
			require.Equal(t, lockCoins(1000)[0], bank.GetBalance(ctx, addr, sdkutil.DenomUakt))
		}
		require.True(t, bank.GetBalance(ctx, acct.GetModuleAddress(escrowmodule.ModuleName), sdkutil.DenomUakt).IsZero())
		spend := &banktypes.MsgSend{FromAddress: wallet.String(), ToAddress: other.String(), Amount: lockCoins(100)}
		_, err = ante(ctx, signedLockTestTx(t, a, ctx, payer, spend, walletKey, payerKey), false)
		require.ErrorIs(t, err, recoverylock.ErrLocked)
	})
}

func signedLockTestTx(t testing.TB, a *app.AkashApp, ctx sdk.Context, payer sdk.AccAddress, msg sdk.Msg, keys ...*secp256k1.PrivKey) sdk.Tx {
	t.Helper()
	return signedLockTestTxMsgs(t, a, ctx, payer, []sdk.Msg{msg}, keys...)
}

func signedLockTestTxMsgs(t testing.TB, a *app.AkashApp, ctx sdk.Context, payer sdk.AccAddress, msgs []sdk.Msg, keys ...*secp256k1.PrivKey) sdk.Tx {
	t.Helper()
	txConfig := a.TxConfig()
	acct := a.Keepers.Cosmos.Acct
	b := txConfig.NewTxBuilder()
	require.NoError(t, b.SetMsgs(msgs...))
	if payer != nil {
		b.SetFeePayer(payer)
	}
	b.SetFeeAmount(lockCoins(3))
	b.SetGasLimit(300_000)
	mode := signing.SignMode_SIGN_MODE_DIRECT
	sigs := make([]signing.SignatureV2, len(keys))
	for i, key := range keys {
		addr := sdk.AccAddress(key.PubKey().Address())
		sigs[i] = signing.SignatureV2{PubKey: key.PubKey(), Data: &signing.SingleSignatureData{SignMode: mode},
			Sequence: acct.GetAccount(ctx, addr).GetSequence()}
	}
	require.NoError(t, b.SetSignatures(sigs...))
	for i, key := range keys {
		addr := sdk.AccAddress(key.PubKey().Address())
		account := acct.GetAccount(ctx, addr)
		var err error
		sigs[i], err = clienttx.SignWithPrivKey(ctx, mode, authsigning.SignerData{
			Address: addr.String(), ChainID: ctx.ChainID(), AccountNumber: account.GetAccountNumber(),
			Sequence: account.GetSequence(), PubKey: key.PubKey(),
		}, b, key, txConfig, account.GetSequence())
		require.NoError(t, err)
	}
	require.NoError(t, b.SetSignatures(sigs...))
	encoded, err := txConfig.TxEncoder()(b.GetTx())
	require.NoError(t, err)
	decoded, err := txConfig.TxDecoder()(encoded)
	require.NoError(t, err)
	return decoded
}

type stakingBankSpy struct {
	stakingtypes.BankKeeper
	delegations int
}

func (b *stakingBankSpy) DelegateCoinsFromAccountToModule(context.Context, sdk.AccAddress, string, sdk.Coins) error {
	b.delegations++
	return nil
}

func TestRecoveryLockStakingBank(t *testing.T) {
	f := newLockFixture(t)
	inner := &stakingBankSpy{}
	bank := recoverylock.StakingBank{L: f.lock, BankKeeper: inner}
	pool := stakingtypes.BondedPoolName
	require.ErrorIs(t, bank.DelegateCoinsFromAccountToModule(f.ctx, f.wallet, pool, lockCoins(1)), recoverylock.ErrLocked)
	require.Zero(t, inner.delegations)
	require.NoError(t, bank.DelegateCoinsFromAccountToModule(f.ctx, f.other, pool, lockCoins(1)))
	require.NoError(t, bank.DelegateCoinsFromAccountToModule(f.ctx.WithBlockHeight(lockHeight-1), f.wallet, pool, lockCoins(1)))
	require.Equal(t, 2, inner.delegations)
}

// benchLockConfig returns a test configuration with n generated listed
// addresses, each with its own generated recovery address, and the eight fee
// payers of recoverylock/testdata.
func benchLockConfig(tb testing.TB, n int) (listed, params []byte) {
	tb.Helper()
	gen := func(prefix string, i int) string {
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", prefix, i)))
		return sdk.AccAddress(digest[:20]).String()
	}
	var list strings.Builder
	recoveries := make(map[string]string, n)
	for i := 0; i < n; i++ {
		wallet := gen("recoverylock-bench-listed", i)
		list.WriteString(wallet + "\n")
		recoveries[wallet] = gen("recoverylock-bench-recovery", i)
	}
	listed = []byte(list.String())
	payers := make([]string, 8)
	for i := range payers {
		payers[i] = placeholderAddress(fmt.Sprintf("recoverylock-test-fee-payer-%d", i)).String()
	}
	params, err := json.Marshal(map[string]any{
		"enabled": true, "recovery_addresses": recoveries, "fee_payers": payers,
		"activation_height": lockHeight, "expiry_time": 4102444800,
		"list_sha256": fmt.Sprintf("%x", sha256.Sum256(listed)), "list_count": n, "test_values": true,
	})
	require.NoError(tb, err)
	return listed, params
}

// BenchmarkRecoveryLockConfig10k measures the startup load and validation of
// a 10,000 address list.
func BenchmarkRecoveryLockConfig10k(b *testing.B) {
	enc := sdkutil.MakeEncodingConfig()
	b.Cleanup(recoverylock.UseTestConfig(benchLockConfig(b, 10_000)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if recoverylock.MustConfig(enc.SigningOptions.AddressCodec, nil, nil) == nil {
			b.Fatal("config disabled")
		}
	}
}

// BenchmarkRecoveryLockAnte measures the full ante handler for an ordinary
// bank send between unlisted accounts, with the committed (disabled)
// configuration and with an active 10,000 address list.
func BenchmarkRecoveryLockAnte(b *testing.B) {
	for _, bc := range []struct {
		name   string
		listed int
	}{{"disabled", 0}, {"active-10k", 10_000}} {
		b.Run(bc.name, func(b *testing.B) {
			if bc.listed > 0 {
				b.Cleanup(recoverylock.UseTestConfig(benchLockConfig(b, bc.listed)))
			}
			a := app.Setup(app.WithHome(b.TempDir()), app.WithGenesis(app.GenesisStateWithValSet))
			b.Cleanup(func() { require.NoError(b, a.Close()) })
			require.Equal(b, bc.listed > 0, a.RecoveryLock != nil)
			ctx := a.NewContext(false).WithBlockHeight(lockHeight).WithBlockTime(time.Unix(1_750_000_000, 0))
			senderKey := placeholderKey("recoverylock-bench-sender")
			sender := sdk.AccAddress(senderKey.PubKey().Address())
			acct, bank := a.Keepers.Cosmos.Acct, a.Keepers.Cosmos.Bank
			acct.SetAccount(ctx, acct.NewAccountWithAddress(ctx, sender))
			require.NoError(b, bank.MintCoins(ctx, minttypes.ModuleName, lockCoins(1_000_000)))
			require.NoError(b, bank.SendCoinsFromModuleToAccount(ctx, minttypes.ModuleName, sender, lockCoins(1_000_000)))
			msg := &banktypes.MsgSend{FromAddress: sender.String(), ToAddress: placeholderAddress("recoverylock-unlisted").String(), Amount: lockCoins(1)}
			tx := signedLockTestTx(b, a, ctx, nil, msg, senderKey)
			ante := a.AnteHandler()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cache, _ := ctx.CacheContext()
				if _, err := ante(cache, tx, false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
