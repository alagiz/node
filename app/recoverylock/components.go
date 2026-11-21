package recoverylock

import (
	"context"

	"github.com/cosmos/cosmos-sdk/baseapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	bmetypes "pkg.akt.dev/go/node/bme/v1"
	"pkg.akt.dev/go/sdkutil"
)

// SendRestriction returns the coin rule for bank.AppendSendRestriction: coins
// leaving a listed address may go to its registered recovery address, or into
// the BME vault as the ACT debit of a MsgBurnACT that the Decorator admitted.
// SendCoins and InputOutputCoins apply this rule to fees, bank sends, IBC
// escrow or burns, module deposits and contract funds.
func (l *Lock) SendRestriction() banktypes.SendRestrictionFn {
	return func(ctx context.Context, from, to sdk.AccAddress, amount sdk.Coins) (sdk.AccAddress, error) {
		sctx := sdk.UnwrapSDKContext(ctx)
		if !l.active(sctx) || !l.cfg.isListed(from) || l.cfg.isRecovery(from, to) ||
			isBurnACTDebit(sctx, from, to, amount) {
			return to, nil
		}
		return nil, lockedf("coins of %s may only go to the recovery address", from)
	}
}

// StakingBank wraps the bank keeper given to x/staking. DelegateCoins does not
// apply the send restriction, so a new delegation from the balance of a
// listed address is rejected at the keeper boundary. The message rules
// stop redelegation and cancel unbonding in ante and message dispatch.
type StakingBank struct {
	stakingtypes.BankKeeper
	L *Lock
}

// DelegateCoinsFromAccountToModule applies the lock to the direct staking
// debit.
func (b StakingBank) DelegateCoinsFromAccountToModule(ctx context.Context, sender sdk.AccAddress, module string, amt sdk.Coins) error {
	l, sctx := b.L, sdk.UnwrapSDKContext(ctx)
	if l.active(sctx) && l.cfg.isListed(sender) {
		return lockedf("%s may not delegate", sender)
	}
	return b.BankKeeper.DelegateCoinsFromAccountToModule(ctx, sender, module, amt)
}

// Router wraps the message router held by the authz, CosmWasm and IBC transfer
// keepers. BaseApp runs top-level messages on its original router after
// ante applies the rules and fee payer requirement.
//
// While the lock is active, the wrapper refuses every dispatched message that
// has a listed signer at any depth, even one the rules would allow. Dispatch
// has no fee payer: with an authz grant made before activation, a contract or
// any grantee could otherwise run allowed messages as a listed wallet without
// the recovery operator (for example fill all unbonding entries with tiny
// undelegations so the operator's undelegation fails). Listed wallets
// therefore act only in top level messages of transactions that pass the
// Decorator. Messages without a listed signer at any depth pass, except that
// errors of the check itself (nesting deeper than MaxDepth, undecodable
// wrapped messages) still refuse.
type Router struct {
	Inner baseapp.MessageRouter
	L     *Lock
}

// Handler returns the inner handler wrapped with the lock check.
func (r Router) Handler(msg sdk.Msg) baseapp.MsgServiceHandler { return r.wrap(r.Inner.Handler(msg)) }

// HandlerByTypeURL returns the inner handler wrapped with the lock check.
func (r Router) HandlerByTypeURL(typeURL string) baseapp.MsgServiceHandler {
	return r.wrap(r.Inner.HandlerByTypeURL(typeURL))
}

func (r Router) wrap(h baseapp.MsgServiceHandler) baseapp.MsgServiceHandler {
	if h == nil {
		return nil
	}
	return func(ctx sdk.Context, msg sdk.Msg) (*sdk.Result, error) {
		if r.L.active(ctx) {
			involved, err := r.L.checkMsg(ctx, msg, 0)
			if err != nil {
				return nil, err
			}
			if involved {
				return nil, lockedf("listed accounts may act only in transactions paid by a fee payer")
			}
		}
		return h(ctx, msg)
	}
}

// burnACTKey marks a transaction whose top-level messages are all MsgBurnACT
// and that passed the Decorator. BaseApp passes the ante context, with its
// values, to message execution. The value lists the ACT debits those messages
// make into the BME vault. Only the coin rule of a listed source reads it, so
// unlisted accounts and inactive windows are unaffected.
type burnACTKey struct{}

type burnACTDebit struct {
	owner sdk.AccAddress
	coin  sdk.Coin
}

var bmeVault = authtypes.NewModuleAddress(bmetypes.ModuleName)

// burnACTDebits returns the debits of a transaction made only of MsgBurnACT,
// or nil when any top-level message is of another type.
func (l *Lock) burnACTDebits(msgs []sdk.Msg) []burnACTDebit {
	debits := make([]burnACTDebit, 0, len(msgs))
	for _, m := range msgs {
		burn, ok := m.(*bmetypes.MsgBurnACT)
		if !ok {
			return nil
		}
		owner, err := l.addr.StringToBytes(burn.Owner)
		if err != nil {
			return nil
		}
		debits = append(debits, burnACTDebit{owner: owner, coin: burn.CoinsToBurn})
	}
	return debits
}

// isBurnACTDebit matches the single uact transfer from the burn owner into
// the BME vault that RequestBurnMint makes for a marked MsgBurnACT.
func isBurnACTDebit(ctx sdk.Context, from, to sdk.AccAddress, amount sdk.Coins) bool {
	if len(amount) != 1 || amount[0].Denom != sdkutil.DenomUact || !to.Equals(bmeVault) {
		return false
	}
	debits, ok := ctx.Value(burnACTKey{}).([]burnACTDebit)
	if !ok {
		return false
	}
	for _, d := range debits {
		if from.Equals(d.owner) && amount[0].Equal(d.coin) {
			return true
		}
	}
	return false
}

// Decorator is the ante decorator. Place it before any fee deduction and
// before signature verification. BaseApp discards ante state on error.
type Decorator struct{ L *Lock }

// AnteHandle rejects a transaction that involves a listed address (as signer
// of a message at any depth, as fee payer or as fee granter) unless every
// message with a listed signer is allowed and the fee is paid by the
// configured fee payer set, without a fee granter, and is not zero.
func (d Decorator) AnteHandle(ctx sdk.Context, tx sdk.Tx, simulate bool, next sdk.AnteHandler) (sdk.Context, error) {
	l := d.L
	if !l.active(ctx) {
		return next(ctx, tx, simulate)
	}
	msgs := tx.GetMsgs()
	if len(msgs) == 0 {
		// Nothing to check, and FeePayer() would panic on an empty signer
		// list. baseapp refuses transactions without messages before the
		// ante handler runs.
		return next(ctx, tx, simulate)
	}
	involved := false
	for _, m := range msgs {
		sub, err := l.checkMsg(ctx, m, 0)
		if err != nil {
			return ctx, err
		}
		involved = involved || sub
	}
	feeTx, ok := tx.(sdk.FeeTx)
	if !ok {
		if involved {
			return ctx, lockedf("transaction without fee information")
		}
		return next(ctx, tx, simulate)
	}
	payer, granter := feeTx.FeePayer(), feeTx.FeeGranter()
	if l.cfg.isListed(payer) || l.cfg.isListed(granter) {
		involved = true
	}
	if involved {
		if !l.cfg.isFeePayer(payer) {
			return ctx, lockedf("fee must be paid by one of the configured fee payers")
		}
		if len(granter) != 0 {
			return ctx, lockedf("fee grants are not allowed")
		}
		if feeTx.GetFee().IsZero() {
			return ctx, lockedf("fee must not be zero")
		}
		if debits := l.burnACTDebits(msgs); len(debits) != 0 {
			ctx = ctx.WithValue(burnACTKey{}, debits)
		}
	}
	return next(ctx, tx, simulate)
}
