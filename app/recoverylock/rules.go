package recoverylock

import (
	"bytes"
	"fmt"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	bmetypes "pkg.akt.dev/go/node/bme/v1"
	deploymenttypes "pkg.akt.dev/go/node/deployment/v1beta4"
)

// checkRule runs only for a listed signer. The codec derives signer bytes from
// the same fields used to authenticate the transaction, including validator
// operator addresses for commission withdrawals.
func (l *Lock) checkRule(ctx sdk.Context, msg sdk.Msg, signer []byte) error {
	switch m := msg.(type) {
	case *banktypes.MsgSend:
		return l.checkDestination(m.ToAddress, signer, false)
	case *banktypes.MsgMultiSend:
		for _, output := range m.Outputs {
			if err := l.checkDestination(output.Address, signer, false); err != nil {
				return err
			}
		}
		return nil
	case *stakingtypes.MsgUndelegate, *distrtypes.MsgWithdrawDelegatorReward, *distrtypes.MsgWithdrawValidatorCommission:
		withdraw, err := l.distr.GetDelegatorWithdrawAddr(ctx, signer)
		if err != nil {
			return err
		}
		if !bytes.Equal(withdraw, signer) && !l.cfg.isRecovery(signer, withdraw) {
			return fmt.Errorf("set the withdraw address to the wallet or its recovery address first")
		}
		return nil
	case *distrtypes.MsgSetWithdrawAddress:
		return l.checkDestination(m.WithdrawAddress, signer, true)
	case *bmetypes.MsgBurnACT:
		// BME pays To on settlement and refunds ACT to Owner on cancellation.
		// Only To == signer is allowed, which is also what the BME validation
		// requires (To == Owner), so both pay the signer.
		to, err := l.addr.StringToBytes(m.To)
		if err != nil {
			return err
		}
		if !bytes.Equal(to, signer) {
			return fmt.Errorf("ACT burn must pay the signer")
		}
		return nil
	case *deploymenttypes.MsgCloseDeployment:
		// The codec derives the signer from ID.Owner, which is part of the
		// deployment key. Escrow returns unspent balances to recorded depositors.
		return nil
	case *wasmtypes.MsgExecuteContract:
		if len(m.Funds) != 0 {
			return fmt.Errorf("contract execute with funds is not allowed")
		}
		recovery, err := l.addr.BytesToString(l.cfg.recoveries[string(signer)])
		if err != nil {
			return err
		}
		return checkContractExit(m.Msg, recovery, func(addr string) error {
			_, err := l.addr.StringToBytes(addr)
			return err
		})
	default:
		return fmt.Errorf("message %s is not allowed for a listed signer", sdk.MsgTypeURL(msg))
	}
}

func (l *Lock) checkDestination(destination string, signer []byte, allowSelf bool) error {
	addr, err := l.addr.StringToBytes(destination)
	if err != nil {
		return err
	}
	if l.cfg.isRecovery(signer, addr) || (allowSelf && bytes.Equal(signer, addr)) {
		return nil
	}
	return fmt.Errorf("destination %s is outside the allowed addresses for this message", destination)
}
