# Recovery lock for akashnet-2

Implementation reference for the Akash team. Base: Akash node v2.2.0, commit `373e027f35df07a6ec5a64607137e52a1c4f408d`, with Cosmos SDK `v0.53.8-akash.1`, IBC Go `v10.7.0`, Wasmd `v0.61.15` and the Akash API `pkg.akt.dev/go v0.5.0-rc2`. The change is one commit on top of v2.2.0.

## What it is

A compiled allow-list for a coordinated halt-height upgrade. From `activation_height`, the wallets in the list may only move funds to their registered recovery address, in transactions whose fee is paid by one of the configured fee payer accounts. Everything else signed by or debiting a listed wallet is refused. Unlisted accounts and module accounts are unaffected. There is no new store, no migration, no upgrade plan and no change to any Akash module: all changes are in the app wiring and the new package `app/recoverylock`.

The window is inclusive at `activation_height` and ends at the first block whose header time is at or after `expiry_time` (Unix seconds). CheckTx evaluates the next height and falls back to the last committed height plus one when its context has no height; with an unknown CheckTx block time it applies the height condition alone. Before activation and after expiry every lock component is a no-op, so the binary executes those blocks exactly like v2.2.0 (verified by replaying whole local chains from genesis on the lock binary, see Tests).

The committed configuration is disabled. A binary built from this commit as it is wires none of the lock components and runs exactly like v2.2.0. A release for the real incident fills in the list and sets `"enabled": true`.

A listed wallet must sign its recovery messages at the top level. The transaction must pay a positive fee from a configured fee payer and have an empty fee granter. Setting the transaction's `Fee.Payer` field makes the SDK require the fee payer's signature in addition to the message signers. A listed wallet's balance cannot fund fees, including fees for a different signer using an older allowance. Transactions that contain no listed signer, payer or granter follow the ordinary ante rules, subject to the active authz unpacking bound. If the current withdraw address is unsafe, confirm a separate `MsgSetWithdrawAddress` transaction before submitting a withdrawal or undelegation. Ante reads the stored address before executing any message in the transaction.

## Layout

| Path | Content |
| --- | --- |
| `app/recoverylock/config.go` | Embedded `listed.txt` and `params.json`, strict parsing, startup validation, the disabled state, `MustConfig`. |
| `app/recoverylock/lock.go` | `Lock`, the active window, message checks with the authz depth bound, `ErrLocked` (codespace `recoverylock`, code 1). |
| `app/recoverylock/rules.go` | The allow-list for listed signers (bank, distribution, staking, deployment close, BME burn, contract exits). |
| `app/recoverylock/contract.go` | Strict JSON shape check for the CosmWasm exits. |
| `app/recoverylock/components.go` | Bank send restriction, staking bank wrapper, dispatch router wrapper, ante decorator, the `MsgBurnACT` vault debit exception. |
| `app/recoverylock/hooks.go` | Staking hooks that reset an unsafe withdraw address before automatic payouts. |
| `app/recoverylock/testbuild_on.go`, `testbuild_off.go` | Selected by the build tag `recoverylock_test` (tests and local test chains only): the `testBuild` constant and, in tagged builds only, `UseTestConfig`. Untagged builds contain no configuration override. |
| `app/recoverylock/listed.txt`, `params.json` | The committed configuration: disabled, no entries. |
| `app/recoverylock/testdata/` | Test values (3 listed wallets, 1 recovery address, 8 fee payers, public placeholder keys) used by the tests and for local chains. |
| `app/ante.go`, `app/app.go`, `app/types/app.go` | Wiring (see Wiring). |
| `app/recoverylock/config_test.go`, `config_tag_test.go`, `app/recoverylock_test.go` | Unit and app tests, benchmarks. `config_test.go` runs in a plain `go test ./...`; the other two need `-tags recoverylock_test`. `make test-full` runs both kinds. |

## Configuration

`app/recoverylock/listed.txt` holds one listed account per line. Blank lines and lines starting with `#` (after whitespace) are ignored. `app/recoverylock/params.json` is parsed strictly: unknown, duplicate or wrong-case keys, wrong value types and trailing data are refused.

Committed (disabled) state:

```json
{
  "enabled": false
}
```

with a `listed.txt` that has no entries (comments only). `"enabled": false` requires every other field to be absent and the list to be empty, so a real list with `enabled` left false makes the node refuse to start instead of running without the lock. With the disabled configuration `MustConfig` returns nil, logs `recovery lock disabled params_sha256=...`, and the app wires no lock component: no ante decorator, no send restriction, no staking bank wrapper, no router wrapper, no staking hooks.

Enabled state fields:

| Field | Meaning |
| --- | --- |
| `enabled` | `true` |
| `recovery_addresses` | Object keyed by the canonical listed address, one recovery address per listed wallet. Several wallets may share a recovery address. A recovery address cannot be a listed wallet. |
| `fee_payers` | 1 to 16 distinct addresses (release builds: at least 8). Not listed, not a recovery address. |
| `activation_height` | First height with the lock (the halt height H). |
| `expiry_time` | Unix seconds. The lock is off from the first block with header time at or after it. Release builds: at most one year after `activation_time_estimate`. |
| `list_count`, `list_sha256` | Count and sha256 of the raw bytes of `listed.txt`. |
| `test_values` | Must be `false` in a release build. |
| `activation_time_estimate`, `estimate_ref_height`, `estimate_ref_time`, `unbonding_seconds` | Release cross-checks, below. |

Every build checks at startup: the list matches `list_sha256` and `list_count`, is not empty and has no duplicates (compared as address bytes); every address is exactly 20 bytes and not a module account or other blocked address; one recovery address per listed wallet; fee payer rules above. A build without the tag `recoverylock_test` (every release build and every plain `go test`) also requires: `test_values` false and none of the known test addresses (the public placeholder keys of `testdata/`); at least 8 fee payers; `unbonding_seconds` between 1814400 (21 days, the akashnet-2 value) and one year; a mainnet reference block (`estimate_ref_height`, `estimate_ref_time`) below the activation height with an implied block time between 1 s and 15 s up to `activation_time_estimate`; and `activation_time_estimate + 2 x unbonding_seconds + 7 days <= expiry_time <= activation_time_estimate + 365 days`. The upper bound stops a mistyped expiry from keeping the wallets locked until another binary ships. Two unbonding periods because the attacker can fill all 7 unbonding entries of a delegation before the halt: the recovery operator's undelegation can then only start when the first of those entries completes, up to one period after activation, and completes one period later. Any failed check panics at startup with `recovery lock config: <reason>`, so a node with a bad list does not start.

Estimate method for a release: take the latest block (height h0, time t0; these are `estimate_ref_height` and `estimate_ref_time`) and the block 10000 heights earlier (time t1); average block time b = (t0 - t1) / 10000; estimate = t0 + (H - h0) x b rounded up to the hour; use the later of this and the planned halt time. Use the live staking `unbonding_time` for `unbonding_seconds`.

At startup an enabled binary logs `recovery lock config` with `mode` (`release` or `test`), `activation_height`, `expiry_time`, `expiry_utc`, `recovery_count`, `params_sha256`, `fee_payers`, `list_count` and `list_sha256`. Publish the sha256 of both files with the release so validators can compare them with that line.

### Why the committed configuration is disabled

The repository's own CI (`make test-full`, `make test-coverage`, the simulations) builds without any extra tag, except one added line in `make test-full` (below). Earlier versions of this branch committed test values that a plain build refuses at startup, so every test that builds the full app failed without `-tags recoverylock_test`. The design now is:

- The committed data is the disabled configuration, so a plain build, a plain `go test ./...` and every CI target run the app exactly as v2.2.0. Every existing upstream test passes unchanged.
- Tests that need the lock are built with `-tags recoverylock_test` and call `recoverylock.UseTestConfig(listed, params)` with the files in `app/recoverylock/testdata/` (or generated data) before building the app, then restore the previous state with the returned function. `UseTestConfig` exists only in builds with that tag (`testbuild_on.go`); an untagged build, including every release, has no way to inject a configuration and no `testing` import. Configuration injected this way is validated with the test rules. The release-rule tests (`config_test.go`) need no tag and run in `make test-full`. `make test-full` (`make/test-integration.mk`, the target of the `tests` job in `.github/workflows/tests.yaml`) has one added line that runs `./app/` and `./app/recoverylock/` again with the repository's build tags plus `recoverylock_test`, so CI also runs the lock's app tests.
- A release replaces `listed.txt` and `params.json` with the real data (`"enabled": true`, `"test_values": false`) and is built with the normal release process and no extra tag. That build refuses test values and the known test addresses at startup (shown under Builds and release).
- The tag `recoverylock_test` is only for tests and local test chains: it lets a binary accept test values in the embedded files. It is never used for a release.

## Message paths

Statuses apply during the active window when a listed wallet is the signer. For operations that can name a different debit account, the reason column also covers a listed source. The ante allowlist is exhaustive. Every other message with a listed signer is blocked, including administrative messages that move no value.

| Module or path | Messages and entry points | Status | Reason |
| --- | --- | --- | --- |
| Bank send | `MsgSend` | Allowed | Recipient must be the sender's registered recovery address. The bank restriction independently enforces the same destination. |
| Bank multi-send | `MsgMultiSend` | Allowed | Every output must go to the listed input's registered recovery address. The SDK supports one input. |
| Bank send to any other destination | `MsgSend`, `MsgMultiSend` with any other output | Blocked | Destination check in ante and bank keeper. |
| Bank configuration | `MsgUpdateParams`, `MsgSetSendEnabled` | Blocked | Governance authority operations outside the allowlist. |
| Fees with registered co-signer | SDK `Fee.Payer` with positive fee and empty `Fee.Granter` | Allowed | SDK verifies the payer's signature and debits the payer. |
| Fees from listed funds or grants | SDK fee payer and fee granter fields, including pre-existing fee grants | Blocked | Ante checks both fields even when every message signer is unlisted. The bank guard also prevents debit to the fee collector. |
| Feegrant | `MsgGrantAllowance`, `MsgRevokeAllowance`, `MsgPruneAllowances` | Blocked | Fee allowance management is outside the allowlist. |
| Authz grants and execution | `MsgGrant`, `MsgRevoke`, listed grantee's `MsgExec` | Blocked | Grant management and wrapper messages are outside the allowlist. |
| Authz nested execution | `MsgExec` by an unlisted grantee, including recursive wrappers and grants created before activation | Blocked | Ante follows the inner messages and applies signer and fee rules at every depth. The keeper's dispatch router refuses every listed inner signer, including an otherwise permitted recovery message. If the transaction passes ante and fails at dispatch, the fee is charged. The maximum depth is six with the outer message at depth zero. |
| Staking principal exit | `MsgUndelegate` | Allowed | Principal matures to the delegator. The current distribution withdraw address must be the wallet or its recovery address. |
| Stake placement and mutation | `MsgCreateValidator`, `MsgDelegate`, `MsgBeginRedelegate`, `MsgCancelUnbondingDelegation` | Blocked | Self-delegation, new delegation, redelegation and reversing an unbonding entry are outside the allowlist. The staking bank wrapper guards the keeper's direct staking debit. |
| Validator controls | `MsgEditValidator`, `MsgUnjail`, staking `MsgUpdateParams` | Blocked | Changing validator settings or status and updating staking parameters are outside the allowlist. |
| Liquid staking module | Tokenize or redeem shares, tokenized share record transfer, related LSM controls | Not applicable | The pinned SDK staking service has no LSM methods. The Akash API staking package is separate from the SDK staking service and its parameter RPC is unregistered. |
| Distribution payouts | `MsgWithdrawDelegatorReward`, `MsgWithdrawValidatorCommission` | Allowed | Current withdraw address must be the listed account or its recovery address. Commission uses the operator address bytes. |
| Withdraw address | `MsgSetWithdrawAddress` | Allowed | May point to the wallet or its recovery address. Any other destination is blocked. |
| Automatic staking payouts | Rewards on delegation changes, commission on validator removal, matured unbonding principal | Allowed | Staking hooks reset a listed wallet's unsafe stored withdraw address before distribution runs. Unbonding principal goes to the delegator. |
| Distribution pool operations | `MsgFundCommunityPool`, `MsgDepositValidatorRewardsPool`, `MsgCommunityPoolSpend`, `MsgUpdateParams` | Blocked | Pool deposits debit the depositor. Pool spending and parameter changes require governance authority and are outside the allowlist. |
| IBC ICS-20 | `MsgTransfer` over IBC v1 and v2, raw v2 `MsgSendPacket` with a transfer payload | Blocked | Transfer debits the sender for escrow or burns vouchers after a module deposit. A listed signer is refused in ante. An unlisted signer cannot send a payload that names a listed wallet as sender: the v2 transfer callback requires the payload sender to equal the packet signer, and the escrow debit from a listed wallet is refused by the coin rule (app test `TestRecoveryLockAppIBCV2SendPacket`). Keeper-generated v2 sends use the wrapped dispatch router. |
| IBC client, channel and relaying | Client, connection and channel RPCs, including v1/v2 receives, acknowledgements and timeouts | Blocked | No listed relayer can sign these. Ordinary relayers can still deliver proof-authorized credits and refunds from module balances or packet escrow. Wasm IBC callbacks act from the contract. |
| IBC interchain accounts and middleware | ICA controller/host, packet forwarding, IBC fee middleware | Not applicable | App module and IBC router registration install only transfer and CosmWasm application routes. |
| Cosmos governance deposits | v1 and v1beta1 `MsgDeposit`, `MsgSubmitProposal` initial deposits including expedited v1 proposals | Blocked | The depositor or proposer funds governance escrow. |
| Cosmos governance other operations | v1 `MsgCancelProposal`, votes in v1 and v1beta1, `MsgExecLegacyContent`, parameter updates | Blocked | Cancellation can distribute already escrowed deposits. Votes and administrative actions are outside the allowlist. Gov's internal router executes proposals as the governance module account. |
| Vesting | `MsgCreateVestingAccount`, `MsgCreatePermanentLockedAccount`, `MsgCreatePeriodicVestingAccount` | Blocked | These debit the funding account and create locked balances. There is no vesting clawback RPC in this SDK. |
| Crisis | `MsgVerifyInvariant` | Not applicable | The type is registered but the app has no handler for it. CheckTx refuses it with `no message handler found` for every signer. |
| CosmWasm exits | Fundless `MsgExecuteContract` with exact cw20 `transfer`, cw721 `transfer_nft`, `decrease_allowance`, `revoke` or `revoke_all` shape | Allowed | Transfers name the canonical recovery address. The other three shapes remove existing rights. Duplicate keys, trailing JSON, unknown fields, wrong value types and optional `expires` are refused. The rule checks message shape and attached funds. |
| Other CosmWasm activity | Funded or other `MsgExecuteContract`, `MsgInstantiateContract`, `MsgInstantiateContract2`, `MsgStoreAndInstantiateContract`, code upload, migration, sudo, admin, label, upload and instantiate permissions, pinning and parameters | Blocked | Instantiate can attach funds and code or control changes can affect assets in contract custody. Only the listed contract exit shapes are allowed. |
| CosmWasm submessages | SDK dispatch, including Any messages through the wrapped router, and native contract IBC/burn handlers | Blocked | Wrapped dispatch refuses listed signers. Native handlers act as the contract. The coin rule applies to any user-source bank debit. |
| Akash deployment close | v1beta4 `MsgCloseDeployment` | Allowed | The deployment ID's owner must sign. Close settles existing lease obligations and refunds unspent escrow to each recorded depositor. Refunds to the listed owner stay locked. Other depositors receive their own refunds as usual. |
| Other Akash deployment operations | v1beta4 `MsgCreateDeployment`, `MsgUpdateDeployment`, `MsgCloseGroup`, `MsgPauseGroup`, `MsgStartGroup`, `MsgUpdateParams` | Blocked | Closing the deployment closes its groups through escrow hooks. No separate group-close transaction is needed. |
| Akash escrow deposits | v1 `MsgAccountDeposit`; authorized deposits via `MsgCreateDeployment` and `MsgCreateBid` | Blocked | The lock checks the signer. The bank restriction checks the actual depositor when a grantee uses a previously granted `DepositAuthorization` without the listed wallet signing the outer message. |
| Akash escrow withdrawal and settlement | `PaymentWithdraw`, `PaymentClose`, `AccountClose`, escrow block processing | Not applicable | These are internal escrow methods. The sole escrow Tx RPC is `MsgAccountDeposit`. Market and deployment entry points are covered separately. Existing escrow deposits can still pay their counterparties or be refunded by ordinary settlement. |
| Akash market bids and leases | v1beta5 `MsgCreateBid`, `MsgCloseBid`, `MsgCreateLease`, `MsgWithdrawLease`, `MsgCloseLease`, `MsgLeaseStartReclaim`, `MsgUpdateParams` | Blocked | Bids can fund or release escrow. Lease operations establish, pay, close or change the status of existing payment obligations. Withdrawal pays the recorded provider. |
| Akash ACT redemption | v1 `MsgBurnACT` | Allowed | The lock permits only `To` equal to the signer, which is also what the pinned BME validation requires (`To == Owner`), so settlement pays AKT to the signer. Cancellation refunds ACT to the signer. The coin rule lets the listed owner's `uact` move into the BME vault only for the exact coin of a `MsgBurnACT` in a transaction that consists of `MsgBurnACT` messages only and passed the decorator. The decorator records those debits in the ante context, which BaseApp passes to message execution. A burn inside a mixed transaction, through authz or without the decorator is refused. No module code is changed. |
| Other Akash BME operations | `MsgBurnMint`, `MsgMintACT`, `MsgFundVault`, `MsgUpdateParams` | Blocked | Governance-only vault funding names a separate source, so its listed-source debit is blocked by the bank guard even under valid governance authority. Pending conversions can settle from module-held balances. |
| Akash sink | `MsgSink`, `MsgRequestVoid`, `MsgReportOutcome`, `MsgVetoVoid`, `MsgRestartTransfer`, `MsgConfirmMint`, `MsgConfirmVoid`, `MsgFinalizeRefund`, `MsgGuardianPause`, `MsgUpdateParams` | Blocked | Deposits debit the sender. Settlement messages can affect whether already locked funds are paid or refunded. Refund execution by an unlisted signer still pays the recorded refund recipient. |
| Akash provider | `MsgCreateProvider`, `MsgUpdateProvider`, `MsgDeleteProvider` | Blocked | Provider registration and metadata are outside the allowlist. Delete is registered but its handler returns an error in this version. |
| Akash certificate | `MsgCreateCertificate`, `MsgRevokeCertificate` | Blocked | Certificate management affects access and makes no direct ledger transfer. |
| Akash audit | `MsgSignProviderAttributes`, `MsgDeleteProviderAttributes` | Blocked | Auditor metadata changes are outside the allowlist. |
| Akash Oracle and Wasm filter | `MsgAddPriceEntry`, Oracle `MsgUpdateParams`, Akash Wasm `MsgUpdateParams` | Blocked | Price feeds and parameters may influence conversion and contract policy. They make no direct debit. |
| Akash take and epochs | Take `MsgUpdateParams`, epochs queries and hooks | Not applicable | `x/take` and its API exist in source but the app does not register their services. Epochs has no Tx Msg service. |
| Other SDK and Akash API packages | Auth, mint, consensus, upgrade, evidence, genutil, params, light client, and API-only packages including client, downtime detector, migrate, types and utils | Not applicable | No additional route for a user to debit their bank balance or move their stake. Any registered administrative, evidence or client messages signed by a listed wallet are still outside the allowlist. |

A listed wallet may receive coins. Already escrowed funds, outstanding interchain packets, previously initiated BME or sink operations, and existing payments can be settled by their modules under the original rules. Contract rights granted before activation remain effective until revoked. The contract exit checks the requested JSON shape without verifying that the contract implements cw20 or cw721 semantics.

## Exits for ACT and deployment escrow

- ACT (`uact`). Bank sends of `uact` are disabled on akashnet-2, so ACT leaves a listed wallet in two transactions, both with a configured fee payer: `MsgBurnACT` to the wallet itself, then, after BME settles the conversion at its next epoch (`min_epoch_blocks`), a `MsgSend` of the AKT payout to the recovery address. A direct payout to a distinct recovery address is impossible because BME's `ValidateBasic` requires `To == Owner` for every account. A burn needs a healthy oracle price for AKT and ACT, and BME refuses it when the circuit breaker status is above the collateral-ratio halt (ACT to AKT stays open at that halt).
- Deployment escrow. The owner signs `MsgCloseDeployment` with a configured fee payer. Escrow's `AccountClose` settles existing lease payments to the provider and `saveAccount` returns each deposit's remaining balance in its own denomination to the recorded depositor. A `uact` refund to the listed owner then follows the ACT path above.

## Wiring

- `app/types/app.go`: `recoverylock.MustConfig` validates the embedded configuration right after the bank keeper is created, and `recoverylock.New` returns nil for the disabled configuration. Only when the lock is non-nil: the bank send restriction is appended, the bank keeper given to staking is wrapped (`StakingBank`), and the message router given to authz, CosmWasm and IBC transfer is wrapped (`Router`). In `SetupHooks` the recovery staking hooks are registered before the distribution and slashing hooks, again only for a non-nil lock.
- `app/ante.go`: `HandlerOptions.RecoveryLock`; when it is non-nil the recovery decorator is inserted immediately after the sink deposit decorator, before transaction size gas, fee deduction and signature handling. The SDK discards cached ante state on rejection.
- `app/app.go`: passes `app.RecoveryLock` into the ante options.
- The original router continues to handle top-level messages, registration and governance. The SDK gov keeper requires every proposal message signer to equal its module account. The BME vault debit still goes through the restricted bank even when the proposal signer is the governance module.
- The ACT exception lives in the lock package only. The decorator adds the list of `MsgBurnACT` debits to the context of an admitted transaction made only of `MsgBurnACT` messages. BaseApp keeps the ante context, with its values, for message execution. The coin rule reads that value only for a listed source while the lock is active, so unlisted accounts, inactive windows and every other module path behave as in v2.2.0.
- SDK `SendCoinsFromAccountToModule` calls `SendCoins`. The direct bank debit used by staking bypasses the send restriction, which is why the staking keeper receives a wrapper.

## Builds, CI and release

All commands run from the repository root with the repository's own environment (`direnv allow`, Go toolchain from `go.mod`, 1.26.2).

### The repository's CI targets

Run on the code of this commit in a clean clone, through `direnv exec . make <target>`, exactly as `.github/workflows/tests.yaml` calls them, with the same `golangci-lint` version the Makefile installs (v2.3.0) and the repository's `.golangci.yaml`. Logs are kept with the test evidence.

| Target | Workflow job | Result |
| --- | --- | --- |
| `make deps-tidy` then `git diff --exit-code go.mod go.sum` | lint-go | pass, no diff |
| `make lint-go` | lint-go | exit 0 (the target uses `--issues-exit-code=0`). With per-linter and same-issue caps removed, v2.2.0 and this commit both report the same 64 findings, all in upstream files; none in the lock code. |
| `make test-full` | tests | pass: 31 packages ok, 0 failures, no build tags, no test cache (before the added tagged line, see below) |
| `make test-coverage` | coverage | pass |
| `make test-sim-nondeterminism`, `test-sim-import-export`, `test-sim-after-import`, `test-sim-fullapp` | sims | pass (TestAppStateDeterminism, TestAppImportExport, TestAppSimulationAfterImport, TestFullAppSimulation) |
| `make lint-shell` | lint-shell | pass |
| `make bins` | build-bins | pass |
| `make release` (goreleaser snapshot) | release-dry-run | see Release build below |
| `make docker-image` | build-bins | not run here, see Release build |
| `make test-vet` | not called by any workflow | fails identically on v2.2.0 and on this commit: `cmd/akash/cmd/genesis.go:122` and `:126`, unkeyed `SendEnabled` fields (upstream code). `go vet` of every package this commit touches passes. |

Rerun on the final code (build revision `70750568`, clean clone, same commands): `make deps-tidy` then `git diff --exit-code go.mod go.sum` pass, no diff; `make lint-go` exit 0, and with the caps removed the same 64 findings as v2.2.0 (diff empty), none in the lock code; `make test-full` pass, 31 packages ok, no build tags. `make test-coverage`, the simulations and `make lint-shell` were not rerun.

The added line in `make test-full`:

```make
	$(GO_TEST) -v $(BUILD_FLAGS) -tags='$(build_tags_cs),recoverylock_test' ./app/ ./app/recoverylock/
```

The second `-tags` overrides the one inside `$(BUILD_FLAGS)` (Go flag parsing keeps the last value). `make test-full` of this commit in a clean clone (2026-11-21, 5 minutes): the untagged line 31 packages ok; the tagged line `ok pkg.akt.dev/node/v2/app` and `ok pkg.akt.dev/node/v2/app/recoverylock`, 16 top-level tests pass (every `TestRecoveryLock*` test, `TestUseTestConfig` and the config tests), 4 skipped (the simulations, which need the `-Enabled` sim flags); 0 failures, exit 0.

Lock tests only:

```sh
go test ./app/recoverylock/
go test -tags recoverylock_test ./app/recoverylock/ ./app/ -run 'RecoveryLock|Config'
go test -tags recoverylock_test ./app/ -run xxx -bench RecoveryLock -count 6
```

### Release build

A release is built by the repository's normal process, with no extra build tag, after `listed.txt` and `params.json` hold the real data:

```sh
sha256sum app/recoverylock/listed.txt app/recoverylock/params.json
make release            # goreleaser-cross; add BUILDOPTS=release only for a real publish
```

Run for this commit in two separate clean clones, each with a local tag `v2.2.1` (the release-dry-run job tags a patch bump the same way), through the repository's own target:

```sh
direnv exec . make release \
  GORELEASER_ARGS="--clean --skip=publish,validate,docker --snapshot --release-notes=.cache/changelog.md" \
  GORELEASER_IMAGE="--cpus=4 --memory=8g ghcr.io/goreleaser/goreleaser-cross:v1.26.2" \
  COSMWASM_OPTIMIZER_IMAGE="--cpus=4 --memory=8g cosmwasm/optimizer:0.17.0"
```

`GORELEASER_ARGS` is the Makefile's own snapshot argument list plus `docker` in `--skip`; the two image variables only add container limits. Results:

- `gen-changelog`, `build-contracts` and goreleaser-cross v1.26.2 ran; goreleaser built darwin amd64, darwin arm64, the darwin universal binary, linux amd64 and linux arm64, the zip, deb and rpm packages and `contracts.zip`, and ended with `release succeeded`.
- Reproducible binaries: the five binaries (darwin amd64, darwin arm64, darwin universal, linux amd64, linux arm64) have the same sha256 in both clones. The zip, deb and rpm files and `contracts.zip` differ between the clones only by the build time stored in them: the files inside the zip archives (the akash binary, the four contract wasm files and their checksum file) and inside the deb packages are byte-identical. The goreleaser config sets no fixed archive timestamp (`mod_timestamp`), so archive checksums depend on the build time. The rpm payload was not unpacked (no rpm tools on the build host). The checksum lists are kept with the evidence.
- `akash version --long` of the linux amd64 binary, run in `ubuntu:noble` (the base image of `_build/akash.Dockerfile`; the release binary needs glibc 2.38 or newer): `version: 2.2.1`, `commit:` this commit, `build_tags: osusergo,netgo,ledger,muslc,gcc`, `go: go version go1.26.2 linux/amd64`, `cosmos_sdk_version: v0.53.8-akash.1`.
- With the committed configuration the release binary starts and logs `recovery lock disabled params_sha256=4a30c9be3f878a9b2ecf59f5e0ada934c9d7e390f09c232597a46d5b4e929a50`.
- Release rules refuse test data: `make bins` (the build-bins target, same tags as the release, no `recoverylock_test`) of this commit with the test data copied in (`enabled` and `test_values` true) stops at startup with `recovery lock config: test values outside a recoverylock_test build`. With `test_values` false and valid release fields (activation height 30000000, reference block 29990000 ten thousand blocks earlier at 6 s per block, 21 day unbonding, expiry two unbonding periods plus 7 days after the estimate) but still the test addresses, it stops with `recovery lock config: list line 2 "akash1p8v4sd70qvvg737wthgeuafc7hh20uf5phg30x" is a known test address`.
- Rerun on the final code in one clean clone (build revision `70750568`, local tag `v2.2.1`, same command): `release succeeded`; linux amd64 `92940adb3043ce16c6f32f80aa5e2ba6d2770b1a237177dafe4689cbb6441e5e`, linux arm64 `68e579c475c1b1738ee1e7be9fb4ba5d19d57f9ba14ebfa29e6a1b86896eaa35`, darwin amd64 `37d19fb6237e95328b9444dec7e48dacc81c94ab0976ef5874a62d66d8c25f48`, darwin arm64 `ceaf8bc31e0e9ac660b204efbb042efa1f9c5622291a7734143d886bf9a579e0`, darwin universal `08a7b971d62848253dcfa7865a505b7bd2d59237d557a26d7cd01f9523be8ae5`. `akash version --long` of the linux amd64 binary in `ubuntu:noble`: `version: 2.2.1`, `commit: 70750568baa03ff0d66b9f93fa11b265fa97b821`, `build_tags: osusergo,netgo,ledger,muslc,gcc`, `go: go version go1.26.2 linux/amd64`. With the committed configuration it logs `recovery lock disabled params_sha256=4a30c9be3f878a9b2ecf59f5e0ada934c9d7e390f09c232597a46d5b4e929a50`. `make bins` refusals on the final code: test data (`test values outside a recoverylock_test build`), release-shaped params with test addresses (`list line 2 "akash1p8v4sd70qvvg737wthgeuafc7hh20uf5phg30x" is a known test address`), and release-shaped params with `expiry_time` one second more than a year after the activation estimate (`expiry_time must be at most one year after the activation estimate`); each binary exits at startup.
- Not run here: the Docker images (`make docker-image` and the docker pipe of `make release`), because the build host has no arm64 emulation and registering it needs a privileged `binfmt` container, a host change; publishing (`BUILDOPTS=release` and a GitHub token); the `network-upgrade` job (needs the `upgrade-tester` runner and a snapshot; this change has no upgrade handler). The `network-upgrade-names` job's checks and the `solana-build-conventions` scripts (with Python 3.12, as on the CI runner) pass.

### Local test chain binaries

For a local chain, copy the files from `app/recoverylock/testdata/` over `listed.txt` and `params.json`, set `activation_height` (and `expiry_time` if wanted) and build with the test tag:

```sh
cp app/recoverylock/testdata/listed.txt app/recoverylock/listed.txt
jq '.activation_height=600' app/recoverylock/testdata/params.json > app/recoverylock/params.json
go build -tags "netgo ledger recoverylock_test" -o akash-lock-test ./cmd/akash
git checkout -- app/recoverylock/listed.txt app/recoverylock/params.json
```

## Operations runbook

### Before the halt

1. Recovery operator, on v2.2.0: a test wallet T that the operator controls (never a compromised wallet) is in the list, with a recovery address the operator also controls, funded with the test amount only. T grants a configured fee payer P an authz `SendAuthorization` with a spend limit of the test amount, T's recovery address as the only allowed recipient, and an expiration well after H. Check with `akash q authz grants <T> <P>`.
2. Publish the binary sha256 (or the commit to rebuild from) and the sha256 of `listed.txt` and `params.json`.
3. Each validator sets `halt-height = H` in `app.toml` and restarts, one validator at a time, then checks that the node is in sync.

### Swap at H, plain service

4. At H the node logs `halt per configuration height H time 0` and `CONSENSUS FAILURE!!!`. The app stays at H-1 with block H stored but not applied. The process does not exit: stop it.
5. Install the lock binary and check its sha256. Set `halt-height = 0` (otherwise the new binary stops at H again). Start.
6. The log shows `recovery lock config ... mode=release` with the published values, then `ABCI Replay Blocks` and `Replay last block using real app`: block H is executed with the lock. The chain resumes at more than 2/3 of the voting power.

### Swap at H with cosmovisor (no upgrade plan)

Tested with cosmovisor v1.7.1 (the version the Makefile pins):

- Create a pre-upgrade hook `<home>/cosmovisor/clear-halt.sh` that sets `halt-height = 0` in `<home>/config/app.toml` and exits 0. Make it executable.
- Service environment: `DAEMON_NAME=akash`, `DAEMON_HOME=<home>`, `DAEMON_ALLOW_DOWNLOAD_BINARIES=false`, `DAEMON_RESTART_AFTER_UPGRADE=true`, `COSMOVISOR_CUSTOM_PREUPGRADE=clear-halt.sh`, and `AKASH_NODE=tcp://127.0.0.1:<rpc port>`.
- `AKASH_NODE` is required. Cosmovisor decides that the upgrade height is reached by running `akash status --home <home>`. In the test the v2.2.0 binary ignored a `node` entry in `<home>/config/client.toml`; without `AKASH_NODE` that command went to `localhost:26657` and fails on any node whose RPC listens elsewhere. Cosmovisor v1.7.1 does not switch while that command fails (`scanner.go`: a `checkHeight` error makes the upgrade check return false). Check it before H: `AKASH_NODE=tcp://127.0.0.1:<rpc port> akash status --home <home>` must print the height.
- Keep `halt-height = H` in `app.toml` (so v2.2.0 never applies H), restart the service with the new environment, then on the running node: `cosmovisor add-upgrade recovery-lock <path to lock binary> --upgrade-height H --force`. It copies the binary to `cosmovisor/upgrades/recovery-lock/bin/akash` and writes `data/upgrade-info.json` (name `recovery-lock`, height H). The Akash app ignores an `upgrade-info.json` whose name is not one of its upgrades.
- At H the node halts, cosmovisor runs the hook (`COSMOVISOR_CUSTOM_PREUPGRADE result argv1=recovery-lock argv2=H`), switches `current` to the upgrade directory and relaunches; the new binary replays H with the lock.

### Check right after the restart (adoption check)

7. Compare the app hash at H+1 with other validators. A match does not prove the swap: until the lock changes a result, a v2.2.0 node computes the same hashes.
8. Before any rescue transaction: P signs and pays `MsgExec{grantee P, msgs [MsgSend from T to T's recovery address, the test amount]}` with a non-zero fee and no fee granter. On the lock binary it passes CheckTx, is included and fails in execution with code 1 and a log ending in `listed accounts may act only in transactions paid by a fee payer: recovery lock`. Only P's fee is charged; T is unchanged. It works whoever proposes the block.
9. Every validator checks that its node still advances after that block. A node still on v2.2.0 executes the inner send and stops at the next block with `wrong Block.Header.AppHash` and `CONSENSUS FAILURE!!!`.

### Recovering a node

- Node still on v2.2.0 after the check (stopped at the check block + 1): stop it, `akash rollback --home <home>` (v2.2.0 binary; it rolls back one height, the check block), install the lock binary, `halt-height = 0`, start. It replays the check block with the lock and follows the chain.
- Validator that missed the halt (no `halt-height`, executed H on v2.2.0): stop it as soon as it is noticed, `akash rollback --home <home>` (rolls back H), install the lock binary, `halt-height = 0`, start. It replays H with the lock and signs again. If it keeps running past H on v2.2.0 instead, it behaves like the full node in the first item: it follows until the first block where the lock changes a result, then stops and needs the same rollback (the stop was tested with a full node, not with a validator).
- New node syncing from genesis: the lock binary alone executes every height from genesis with the same app hashes (blocks before activation and after expiry run as on v2.2.0). State sync was not tested.

### Abort

Only before the chain resumes on the lock binary (fewer than 2/3 have swapped): every node that already executed H on the lock binary runs `akash rollback --home <home>` and starts on v2.2.0 with `halt-height = 0` (or a new halt height for a second attempt). Nodes that only halted start on v2.2.0 with `halt-height = 0`. Under cosmovisor, stop the service, point `cosmovisor/current` back at `genesis` (or run v2.2.0 outside cosmovisor), and remove `data/upgrade-info.json`. Block H then executes on v2.2.0 everywhere. A later attempt needs a new halt height and a binary whose `activation_height` is that height.

### Expiry

At the first block whose header time is at or after `expiry_time` the lock is off: listed wallets act like any other account and the binary behaves as v2.2.0; a v2.2.0 node stays in sync from there (chain C). Extending the window needs a new binary; if every validator swaps before the old expiry, no halt is needed.

### Recovery operator after the check transaction

Fix withdraw addresses, withdraw rewards and commission, undelegate everything, close deployments, burn ACT to the wallet and send the AKT payout after settlement, remove cw20 and cw721 rights, send balances to each wallet's recovery address. All fees are paid by the fee payer set, with `Fee.Payer` set and both the wallet and the fee payer signing.

## Tests

Every result below was produced on 2026-11-20 with Go 1.26.2, except the copy of mainnet state and the `make test-full` run with the added tagged line (2026-11-21, on this commit's code). The unit and app tests, the CI targets marked as rerun, the release build and the local chain rows under "Rerun on the final code" ran on the final code: build revision `70750568baa03ff0d66b9f93fa11b265fa97b821`, whose code tree is identical to this commit (only this document changed afterwards). The full local chains A, B and C, the remaining CI targets and the performance runs ran on the previous revision of this change, before the review cleanups (removed `testing` import and test-only override in untagged builds, `MsgBurnACT` limited to `To` = signer, one distribution keeper reference in the hooks, removed unreachable context guard, release upper bound on `expiry_time`, new IBC v2 app test). Those cleanups change no behaviour on the paths that were not rerun.

### Unit and app tests

`app/recoverylock/config_test.go` runs in a plain `go test ./...` and in the untagged line of `make test-full` (config validation, strict params parsing, the disabled committed configuration, the test data under release rules). `app/recoverylock/config_tag_test.go` and `app/recoverylock_test.go` run with `-tags recoverylock_test` (the second line of `make test-full`): blocked and allowed paths with Akash's registered codec, fee payer, fee granter and zero fee rules, the active window, nested authz and wrapped dispatch, staking guard, staking hooks, app wiring with the real ante chain, signatures and bank state, `MsgBurnACT` destinations, ACT debit and settlement, refusal of a direct vault debit, refusal of a non-owner deployment close, a raw IBC v2 `MsgSendPacket` by an unlisted signer with a listed payload sender (`TestRecoveryLockAppIBCV2SendPacket`), deployment close refunds to the listed owner and to another depositor, config validation, strict params parsing, the disabled committed configuration (`TestRecoveryLockDisabledByDefault`, `TestEmbeddedConfigDisabled`, `TestDisabledConfigCarriesNoData`), the test data under release rules (`TestTestDataConfig`: refused) and `UseTestConfig`.

### Local networks

Three local chains (`lockdev-1`, four validators with equal stake plus full nodes, `timeout_commit` 1 s, about 1.6 s per block on the shared test machine, unbonding time 300 s, minimum gas price 0), every node in its own capped systemd scope. Base binary: v2.2.0 (`373e027f`), tags `netgo ledger`, sha256 `f842cb1727d3d63009f0e71120bdf11999f8e04d3f27b1178b33abec9349dde0`. Lock binaries: this code with the files of `app/recoverylock/testdata/` and the activation height of each test, tags `netgo ledger recoverylock_test`:

| Binary | activation_height | expiry_time | params sha256 | binary sha256 |
| --- | --- | --- | --- | --- |
| lock-a | 600 | 4102444800 | `b9a6b8835a4f486cca7008fd059688b5c1493daac0accee6be0cfdf7f22c7fe9` | `cf94b71a578285b81904a347a02f0e625f071f11f6b7eb75aece5275cbfa4b9d` |
| lock-b | 200 | 4102444800 | `c144d49606877891f884ab711a5f2f4210b5e8b4d5f60002062c69e5753a7669` | `e155670865705d7b01aeef99b6b7a0de010aa19394bc499494c87861a7e2ebb7` |
| lock-c1 | 150 | 4102444800 | `0fd25adf94f6b599f161c70b45b37d7c4541c65c3bf3511acf62429fff4b3587` | `02b298813b1da02aacfc6f8201b6d11461f4aaaf621ba6d46d65a2e0f3e26572` |
| lock-c2 | 300 | 1795203561 (2026-11-20T19:39:21Z) | `6170f79630ee2e9760791d755caf2d4554bc2c009be0ab8c3da99fd3d681832b` | `287d4425811b7e3848ec2436edf8b2d42b87657f619453cb6be20079f5ea5c66` |

The list is the test list (`list_count=3`, `list_sha256=66cdc7fbdfbc7dd0331de7b8f90274e89ceaa84d40c895b74792e8edda665941`). Test accounts use public placeholder keys: listed L0 `akash1p8v4sd70qvvg737wthgeuafc7hh20uf5phg30x`, L1 `akash1udx7a8ujgc6h95qt9cpxdrcu4t5nkz8sn6dhnc`, L2 `akash19pl7sv85wc6hhrn9rtgu3p96yczdwft5m3uxaj` (also a validator operator), recovery address R `akash1ksdgkxhjctrgxrz6lw232f92fq7sr3hp6ccp4u`, fee payers P0 `akash1j03vl40hflp3vqnww596qt0vxp9arzzwgrlzhs`, P1 `akash14a2cwlzs3xhkaysy6j98yp2c6fgnr8wx3dzd9g`, P2 `akash1qpwtnpt5fj8fgt9t62ymeunr2hax4f5g0ej64r`, unlisted U `akash17w3n0356c0u77jjcunzsyjva9kx62nlaypdjmj`. Local only: attacker X `akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w`, provider PV `akash16d5y3kx3qdnrjr5dx6d3dx7umwypcq3au27hsh`, oracle source F `akash106hfgq4jepjz0cwpajrhf900sxejvt69uhsd3s`.

#### Chain A: message paths and validator operations (H = 600)

Nodes: node0 to node2 validators under cosmovisor v1.7.1 with `halt-height = 600`, the pre-upgrade hook and `cosmovisor add-upgrade recovery-lock <lock-a> --upgrade-height 600`; node3 a validator without `halt-height` (missed the halt), node4 a full node on v2.2.0 (missed the swap), node5 added later for the replay.

Prepared on v2.2.0 before the halt, as an attacker could (28 setup transactions, all code 0): certificates, a provider (PV), a cw20 code and contract with balances for L0 and X and an allowance from L0 to X, delegations of L0 and L1, L2 as a validator, L0's withdraw address set to X, authz grants from L0 to X (generic `MsgSend`, `MsgExec`, `MsgUndelegate`) and from U to L0 and X, the adoption grant from L0 to P0 (`MsgSend`, 1000000uakt, allow-list R), fee allowances L0 to X and U to L0, an escrow `DepositAuthorization` from L0 to X, L0 deployments dseq 1 (leased to PV) and dseq 2, a deployment of U, a text proposal of U in its deposit period.

Validator operations:

| ID | Check | Result |
| --- | --- | --- |
| CV-SWAP | node0 to node2 swap through cosmovisor, no upgrade plan | pass. Each logged `halt per configuration height 600 time 0` at 19:09:42 UTC, `COSMOVISOR_CUSTOM_PREUPGRADE result argv1=recovery-lock argv2=600`, `upgrade detected, relaunching`, then `recovery lock config activation_height=600 ... list_count=3 ... mode=test params_sha256=b9a6b8835a4f486cca7008fd059688b5c1493daac0accee6be0cfdf7f22c7fe9`; `cosmovisor/current` points to `upgrades/recovery-lock`, binary sha256 `cf94b71a578285b81904a347a02f0e625f071f11f6b7eb75aece5275cbfa4b9d`, `halt-height = 0`. Chain at 603 at 19:09:51 UTC. |
| MISS-H | node3 (no halt-height) executes H on v2.2.0 | pass. App height 600, app hash `65B844803FB43E4F9F13E6E1EB765A09514AFFCFF251CBA5A29BF2131D2656A2`. Block 600 held no transaction the lock changes, so this equals the lock result; in general it does not. node3 was stopped before the chain resumed. |
| node3 rollback | `akash rollback --home node3` with v2.2.0, then start on lock-a | pass. `Rolled back state to height 599 and hash 82C79E927C8A793017F2BD9CF8CAC64C6A2A4C89550850E7D396C7D1C41F4804`; node3 replayed 600 with the lock, was at 610 at 19:10:06 UTC and signed 10 of the last 10 commits up to 854 (NODE3-SIGNS). |
| ADOPT | adoption check, first lock-relevant transaction after activation | pass. P0 `MsgExec(L0 MsgSend 1000000uakt to R)`, tx `1FDE7D660D1E0B6EC9E759A3A86954A7237BFFEEA2A3ADE165F84B4E1A37990F`, block 612, code 1, gas used 67013, log ends `listed accounts may act only in transactions paid by a fee payer: recovery lock`. L0 38996531694uakt and R 274584uakt unchanged on the lock nodes. node4 (v2.2.0) executed the send (L0 38995531694, R 1274584) and at 613 logged `wrong Block.Header.AppHash. Expected 69E2896AD66C9DC9DE35A4736C0FD9A1869EEFE5989CC5AB2BBDCBE195FCF8A5, got DAC572797650FC869067ECD9382700452E9B5EB4DDC955D88E9159D69A0D56CF` and stopped. |
| NODE4-REJOIN | `akash rollback --home node4` with v2.2.0, then start on lock-a | pass. `Rolled back state to height 611 and hash 7DD010DDE802B76CDD09AABD45D90C8A539387C3C81A8759AACE95A034CF0DAC`; node4 replayed 612 with the lock; app hash at 613 `DAC572797650FC869067ECD9382700452E9B5EB4DDC955D88E9159D69A0D56CF` equal to node0; it followed the chain. |
| K06m, K12, K13 | matured unbonding principal | pass. L1's 5000 AKT unbonding from the post phase matured and paid L1 (25015612598 to 30015612598uakt); L1 sent 5000 AKT to R with fee payer P2 (code 0, block 854, tx `D6EC06C698AF737CCE4862BBD34659B928CDF8157768A16276A14B2E487FE48A`); its own-fee send to X was refused at CheckTx. |
| AK18 | PV `MsgCloseBid` on L0's dseq 1 | pass. Code 0, block 679; L0's balances unchanged. |
| REPLAY | node5, a new node with only lock-a, syncs from genesis | pass. Reached the tip (870) in 51 s with no app hash error; app hashes equal to node0 at 1, 599, 600, 601, 612, 613 and 870; its finalized app hashes for 880 to 898 equal node0's. |
| CONS | app hashes | node0 to node4 equal at 599, 600, 601, 612, 613 and 888; blocks continued. |

Path results: every transaction was sent once on v2.2.0 before the halt and once on the lock binary after activation, through node0. Listed signers used fee payer P1 even for blocked messages, so a refusal shows the message rule and not only the fee rule. Refusals at CheckTx never enter a block and charge no fee. 74 of 74 rows before activation and 82 of 82 rows after activation matched the expectation. Two notes: W02 before activation was sent a second time after a client-side broadcast error (`net/http: nil Context`) and then succeeded (block 146); the harness expected AK03 after activation to be refused (the rule of an earlier version of this branch), the transaction succeeded as the current rules allow (owner close with a fee payer, escrow refund 5000000uact to L0), and the row was re-recorded from the same transaction.

| ID | Transaction | Expected after activation | Before activation (v2.2.0) | After activation (lock) | Result |
| --- | --- | --- | --- | --- | --- |
| B01 | L0 MsgSend 1uakt to recovery R, fee payer P1 | succeeds | code 0, h38 | code 0, h623 | pass |
| B02 | L0 MsgSend 1uakt to attacker X, fee payer P1 | refused at CheckTx by the lock | code 0, h39 | code 1, CheckTx: destination akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w is outside the allowed addresses for this message | pass |
| B03 | L0 MsgSend 1uakt to attacker X, own fee | refused at CheckTx by the lock | code 0, h40 | code 1, CheckTx: destination akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w is outside the allowed addresses for this message | pass |
| B04 | L0 MsgSend 1uakt to R, own fee (listed wallet pays fee) | refused at CheckTx by the lock | code 0, h41 | code 1, CheckTx: fee must be paid by one of the configured fee payers | pass |
| B05 | L0 MsgMultiSend 1uakt to R twice, fee payer P1 | succeeds | code 0, h42 | code 0, h625 | pass |
| B06 | L0 MsgMultiSend to R and X, fee payer P1 | refused at CheckTx by the lock | code 0, h43 | code 1, CheckTx: destination akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w is outside the allowed addresses for this message | pass |
| B07 | unlisted U MsgSend 1uakt to listed L0 (receiving) | succeeds | code 0, h45 | code 0, h627 | pass |
| B08 | unlisted U MsgSend with listed L0 as fee payer | refused at CheckTx by the lock | code 0, h46 | code 1, CheckTx: fee must be paid by one of the configured fee payers | pass |
| B09 | L0 MsgSend to R, fee payer P1, zero fee | refused at CheckTx by the lock | code 0, h47 | code 1, CheckTx: fee must not be zero | pass |
| B10 | X MsgSend with fee granter L0 (pre-activation allowance) | refused at CheckTx by the lock | code 0, h48 | code 1, CheckTx: fee must be paid by one of the configured fee payers | pass |
| B11 | L0 MsgSend to R with fee granter U | refused at CheckTx by the lock | code 0, h49 | code 1, CheckTx: fee must be paid by one of the configured fee payers | pass |
| B12 | L0 MsgSend 1uact to R, fee payer P1 (uact sends are disabled chain-wide) | fails, not by the lock | code 5, h50: uact transfers are currently disabled: send transactions are disabled | code 5, h630: uact transfers are currently disabled: send transactions are disabled | pass |
| B13 | unlisted U MsgSend to X, fee payer P1 (fee payer serves unlisted) | succeeds | code 0, h51 | code 0, h631 | pass |
| D01 | L0 MsgWithdrawDelegatorReward while withdraw address is X | refused at CheckTx by the lock | code 0, h52 | code 1, CheckTx: set the withdraw address to the wallet or its recovery address first | pass |
| D02 | L0 MsgSetWithdrawAddress to X | refused at CheckTx by the lock | code 0, h53 | code 1, CheckTx: destination akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w is outside the allowed addresses for this message | pass |
| K01 | L0 MsgUndelegate 1 AKT from val1 while withdraw address is X | refused at CheckTx by the lock | code 0, h54 | code 1, CheckTx: set the withdraw address to the wallet or its recovery address first | pass |
| D03 | L0 MsgSetWithdrawAddress to R | succeeds | code 0, h55 | code 0, h633 | pass |
| D04 | L0 MsgWithdrawDelegatorReward to R | succeeds | code 0, h56 | code 0, h634 | pass |
| K02 | L0 MsgUndelegate 1 AKT from val0 (withdraw address R) | succeeds | code 0, h58 | code 0, h635 | pass |
| D05 | L0 MsgSetWithdrawAddress to itself | succeeds | code 0, h60 | code 0, h636 | pass |
| D06 | L0 MsgFundCommunityPool 1uakt | refused at CheckTx by the lock | code 0, h61 | code 1, CheckTx: message /cosmos.distribution.v1beta1.MsgFundCommunityPool is not allowed for a listed signer | pass |
| K03 | L0 MsgDelegate 1 AKT to val0 | refused at CheckTx by the lock | code 0, h62 | code 1, CheckTx: message /cosmos.staking.v1beta1.MsgDelegate is not allowed for a listed signer | pass |
| K04 | L0 MsgBeginRedelegate 1 AKT val1 to val2 | refused at CheckTx by the lock | code 0, h63 | code 1, CheckTx: message /cosmos.staking.v1beta1.MsgBeginRedelegate is not allowed for a listed signer | pass |
| K05 | L0 MsgCancelUnbondingDelegation (own unbonding entry) | refused at CheckTx by the lock | code 0, h64 | code 1, CheckTx: message /cosmos.staking.v1beta1.MsgCancelUnbondingDelegation is not allowed for a listed signer | pass |
| K06 | L1 MsgUndelegate 5000 AKT from val1 (withdraw address itself) | succeeds | code 0, h66 | code 0, h639 | pass |
| K07 | listed operator L2 MsgEditValidator | refused at CheckTx by the lock | code 0, h67 | code 1, CheckTx: message /cosmos.staking.v1beta1.MsgEditValidator is not allowed for a listed signer | pass |
| K08 | listed operator L2 MsgUnjail (not jailed) | refused at CheckTx by the lock | code 5, h68: validator not jailed; cannot be unjailed | code 1, CheckTx: message /cosmos.slashing.v1beta1.MsgUnjail is not allowed for a listed signer | pass |
| K09 | listed operator L2 MsgWithdrawValidatorCommission plus rewards | succeeds | code 0, h69 | code 0, h641 | pass |
| K10 | listed L1 MsgCreateValidator | refused at CheckTx by the lock | not run | code 1, CheckTx: message /cosmos.staking.v1beta1.MsgCreateValidator is not allowed for a listed signer | pass |
| K11 | unlisted U MsgDelegate 1 AKT to val0 | succeeds | code 0, h71 | code 0, h643 | pass |
| A01 | X MsgExec(L0 MsgSend to X) with pre-activation grant | refused at CheckTx by the lock | code 0, h73 | code 1, CheckTx: destination akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w is outside the allowed addresses for this message | pass |
| A02 | X MsgExec(L0 MsgSend to R), X pays fee | refused at CheckTx by the lock | code 0, h74 | code 1, CheckTx: fee must be paid by one of the configured fee payers | pass |
| A03 | nested: X MsgExec(MsgExec by L0 (MsgSend U to X)) | refused at CheckTx by the lock | code 0, h75 | code 1, CheckTx: message /cosmos.authz.v1beta1.MsgExec is not allowed for a listed signer | pass |
| A04 | listed grantee L0 MsgExec(U MsgSend to X), fee payer P1 | refused at CheckTx by the lock | code 0, h77 | code 1, CheckTx: message /cosmos.authz.v1beta1.MsgExec is not allowed for a listed signer | pass |
| A05 | X MsgExec(L0 MsgUndelegate 1uakt): unbonding entry fill | refused at CheckTx by the lock | code 0, h78 | code 1, CheckTx: fee must be paid by one of the configured fee payers | pass |
| A06 | L0 MsgGrant (generic MsgVote to X) | refused at CheckTx by the lock | code 0, h79 | code 1, CheckTx: message /cosmos.authz.v1beta1.MsgGrant is not allowed for a listed signer | pass |
| A07 | L0 MsgRevoke (MsgVote grant) | refused at CheckTx by the lock | code 0, h80 | code 1, CheckTx: message /cosmos.authz.v1beta1.MsgRevoke is not allowed for a listed signer | pass |
| A08 | unlisted: X MsgExec(U MsgSend to X) | succeeds | code 0, h81 | code 0, h647 | pass |
| F01 | L0 MsgGrantAllowance to F | refused at CheckTx by the lock | code 0, h82 | code 1, CheckTx: message /cosmos.feegrant.v1beta1.MsgGrantAllowance is not allowed for a listed signer | pass |
| F02 | L0 MsgRevokeAllowance of F | refused at CheckTx by the lock | code 0, h83 | code 1, CheckTx: message /cosmos.feegrant.v1beta1.MsgRevokeAllowance is not allowed for a listed signer | pass |
| G01 | L0 gov MsgDeposit 100000uakt on proposal 1 | refused at CheckTx by the lock | code 0, h84 | code 1, CheckTx: message /cosmos.gov.v1.MsgDeposit is not allowed for a listed signer | pass |
| G02 | unlisted U gov MsgDeposit 100000uakt on proposal 1 | succeeds | code 0, h85 | code 0, h650 | pass |
| G03 | L0 gov MsgSubmitProposal with 1 AKT initial deposit | refused at CheckTx by the lock | code 0, h86 | code 1, CheckTx: message /cosmos.gov.v1.MsgSubmitProposal is not allowed for a listed signer | pass |
| G04 | L0 gov MsgVote (proposal in deposit period) | refused at CheckTx by the lock | code 3, h87: 1: inactive proposal | code 1, CheckTx: message /cosmos.gov.v1.MsgVote is not allowed for a listed signer | pass |
| G05 | L0 gov MsgCancelProposal of its proposal 2 | refused at CheckTx by the lock | not run | code 1, CheckTx: message /cosmos.gov.v1.MsgCancelProposal is not allowed for a listed signer | pass |
| V01 | L0 MsgCreateVestingAccount 1000uakt | refused at CheckTx by the lock | code 0, h89 | code 1, CheckTx: message /cosmos.vesting.v1beta1.MsgCreateVestingAccount is not allowed for a listed signer | pass |
| V02 | L0 MsgCreatePermanentLockedAccount 1000uakt | refused at CheckTx by the lock | code 0, h92 | code 1, CheckTx: message /cosmos.vesting.v1beta1.MsgCreatePermanentLockedAccount is not allowed for a listed signer | pass |
| V03 | unlisted U MsgCreateVestingAccount 1000uakt | succeeds | code 0, h95 | code 0, h654 | pass |
| C01 | L0 crisis MsgVerifyInvariant (no handler registered in this app) | fails, not by the lock | code 6, CheckTx: no message handler found for *types.MsgVerifyInvariant: unknown request | code 6, CheckTx: no message handler found for *types.MsgVerifyInvariant: unknown request | pass |
| I01 | L0 IBC MsgTransfer on transfer/channel-0, fee payer P1 | refused at CheckTx by the lock | code 35, h99: send packet failed for source id: channel-0: counterparty not found for client: channel-0: counterparty not found | code 1, CheckTx: message /ibc.applications.transfer.v1.MsgTransfer is not allowed for a listed signer | pass |
| I02 | unlisted U IBC MsgTransfer on transfer/channel-0 (fails on missing channel, not on the lock) | fails, not by the lock | code 35, h101: send packet failed for source id: channel-0: counterparty not found for client: channel-0: counterparty not found | code 35, h656: send packet failed for source id: channel-0: counterparty not found for client: channel-0: counterparty not found | pass |
| W01 | L0 cw20 transfer 100 to R, fee payer P1 | succeeds | code 0, h103 | code 0, h657 | pass |
| W02 | L0 cw20 transfer 100 to X, fee payer P1 | refused at CheckTx by the lock | code 0, h146 | code 1, CheckTx: contract message "transfer" field recipient must be the recovery address | pass |
| W03 | L0 cw20 transfer to R with 1uakt funds attached | refused at CheckTx by the lock | code 0, h108 | code 1, CheckTx: contract execute with funds is not allowed | pass |
| W04 | L0 cw20 burn 1 | refused at CheckTx by the lock | code 0, h110 | code 1, CheckTx: contract message "burn" is not allowed (only transfer and transfer_nft to the recovery address, decrease_allowance, revoke, revoke_all) | pass |
| W05 | L0 MsgStoreCode cw20 | refused at CheckTx by the lock | code 0, h113 | code 1, CheckTx: message /cosmwasm.wasm.v1.MsgStoreCode is not allowed for a listed signer | pass |
| W06 | L0 MsgInstantiateContract code 1 | refused at CheckTx by the lock | code 0, h115 | code 1, CheckTx: message /cosmwasm.wasm.v1.MsgInstantiateContract is not allowed for a listed signer | pass |
| W07 | X cw20 transfer_from L0 100 (allowance granted before activation, contract right) | succeeds | code 0, h116 | code 0, h660 | pass |
| W08 | L0 cw20 decrease_allowance for X, fee payer P1 | succeeds | code 0, h117 | code 0, h661 | pass |
| W09 | X cw20 transfer_from L0 after decrease_allowance (no allowance) | fails, not by the lock | not run | code 5, h662: No allowance for this account: execute wasm contract failed | pass |
| W10 | unlisted U cw20 transfer 1 to X | succeeds | code 0, h118 | code 0, h663 | pass |
| AK01 | L0 MsgCreateDeployment dseq 20 deposit 5000000uact | refused at CheckTx by the lock | code 0, h120 | code 1, CheckTx: message /akash.deployment.v1beta4.MsgCreateDeployment is not allowed for a listed signer | pass |
| AK02 | L0 escrow MsgAccountDeposit 1000000uact on dseq 1 | refused at CheckTx by the lock | code 0, h121 | code 1, CheckTx: message /akash.escrow.v1.MsgAccountDeposit is not allowed for a listed signer | pass |
| AK03 | L0 MsgCloseDeployment dseq 2, fee payer P1 (owner close is allowed) | succeeds | code 0, h123 | code 0, h665, escrow refund 5000000uact to L0 | pass |
| AK04 | X MsgCreateDeployment dseq 21 funded only by L0 DepositAuthorization | included, refused in delivery by the lock | code 0, h124 | code 1, h666: coins of akash1p8v4sd70qvvg737wthgeuafc7hh20uf5phg30x may only go to the recovery address | pass |
| AK05 | provider PV MsgWithdrawLease on L0 dseq 1 (escrow pays provider) | succeeds | code 0, h125 | code 0, h667 | pass |
| AK06 | L0 MsgCloseLease dseq 1 | refused at CheckTx by the lock | not run | code 1, CheckTx: message /akash.market.v1beta5.MsgCloseLease is not allowed for a listed signer | pass |
| AK07 | L1 MsgCreateProvider | refused at CheckTx by the lock | code 0, h126 | code 1, CheckTx: message /akash.provider.v1beta4.MsgCreateProvider is not allowed for a listed signer | pass |
| AK08 | L0 MsgCreateBid on U dseq 2 (L0 as provider) | refused at CheckTx by the lock | code 0, h127 | code 1, CheckTx: message /akash.market.v1beta5.MsgCreateBid is not allowed for a listed signer | pass |
| AK09 | L0 MsgUpdateProvider | refused at CheckTx by the lock | code 0, h128 | code 1, CheckTx: message /akash.provider.v1beta4.MsgUpdateProvider is not allowed for a listed signer | pass |
| AK10 | L0 audit MsgSignProviderAttributes for PV | refused at CheckTx by the lock | code 0, h129 | code 1, CheckTx: message /akash.audit.v1.MsgSignProviderAttributes is not allowed for a listed signer | pass |
| AK11 | L0 oracle MsgAddPriceEntry (L0 is not a source) | refused at CheckTx by the lock | code 4, h131: source akash1p8v4sd70qvvg737wthgeuafc7hh20uf5phg30x is not authorized oracle provider: unauthorized | code 1, CheckTx: message /akash.oracle.v2.MsgAddPriceEntry is not allowed for a listed signer | pass |
| AK12 | L0 BME MsgMintACT 10000000uakt | refused at CheckTx by the lock | code 6, h133: circuit breaker is active | code 1, CheckTx: message /akash.bme.v1.MsgMintACT is not allowed for a listed signer | pass |
| AK12b | L0 BME MsgBurnACT 1000000uact to itself, fee payer P1 (burn-only tx: allowed) | succeeds | code 0, h134 | code 0, h673 | pass |
| AK13 | L0 sink MsgSink 1000000uakt | refused at CheckTx by the lock | code 15, h135: deposits disabled | code 1, CheckTx: message /akash.sink.v1.MsgSink is not allowed for a listed signer | pass |
| AK14 | L0 MsgRevokeCertificate | refused at CheckTx by the lock | not run | code 1, CheckTx: message /akash.cert.v1.MsgRevokeCertificate is not allowed for a listed signer | pass |
| AK15 | unlisted U MsgCreateDeployment dseq 22 | succeeds | code 0, h136 | code 0, h675 | pass |
| AK16 | unlisted U escrow MsgAccountDeposit 1000000uact on dseq 22 | succeeds | code 0, h137 | code 0, h676 | pass |
| AK17 | unlisted U MsgCloseDeployment dseq 22 | succeeds | code 0, h138 | code 0, h677 | pass |
| AK18 | provider PV MsgCloseBid on L0 dseq 1: lease closes, escrow stays with the deployment | succeeds | not run | code 0, h679 | pass |
| K12 | L1 MsgSend of matured 5000 AKT to R, fee payer P2 | succeeds | not run | code 0, h854 | pass |
| K13 | L1 MsgSend of matured AKT to X, own fee | refused at CheckTx by the lock | not run | code 1, CheckTx: destination akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w is outside the allowed addresses for this message | pass |

Before activation, rows I01, I02 and K08 failed for their own reasons (no IBC counterparty, validator not jailed), C01 has no handler in either binary, B12 fails because `uact` sends are disabled, G04 votes on a proposal still in its deposit period, AK11 is signed by an account that is not an oracle source, AK12 hit the BME circuit breaker of the new local vault and AK13 hit the sink default with deposits disabled. After activation the lock refuses all of these listed-signer messages at CheckTx before their handlers run, except C01, which the app refuses as unroutable, and B12, which is allowed by the lock and then refused by the bank send setting. Rows marked not run before activation only make sense after it.

#### Chain B: ACT, deployment close, rewards (H = 200)

node0 to node3 halted at 200 and were swapped by hand (stop, replace binary, `halt-height = 0`, start) to lock-b; node4 stayed on v2.2.0. Before activation: L0 withdraw address X, U delegated 10000 AKT to val0 with withdraw address T `akash1mm0vtpwmtf2gdr840hgfq9ll9kxrevmr762xxt`, L0 deployments dseq 1 (lease with PV) and dseq 2 with 5000000uact each, U and L0 burned 1000000uact on v2.2.0. BME genesis defaults (`min_epoch_blocks` 10), F fed AKT/USD 1.0 before each burn. All transactions below are after activation, fee payer P1 unless noted.

| ID | Transaction | Expected | Result |
| --- | --- | --- | --- |
| BU01 | L0 MsgBurnACT 1000000uact to itself, fee payer P1 | succeeds | pass: code 0, h215; L0 before 40000955000 49989000000 after 40001955000 49988000000 (uakt 1000000, uact -1000000) |
| BU02 | L0 MsgBurnACT 1000000uact with to=X, fee payer P1 | fails, not by the lock | pass: CheckTx code 3: owner and to addresses must match: unauthorized |
| BU03 | L0 MsgBurnACT 1000000uact with to=R (recovery), fee payer P1 | fails, not by the lock | pass: CheckTx code 3: owner and to addresses must match: unauthorized |
| BU04 | L0 MsgBurnACT 1000000uact to itself, own fee | refused at CheckTx by the lock | pass: CheckTx code 1: fee must be paid by one of the configured fee payers |
| BU05 | L0 one tx [MsgBurnACT to itself, MsgSend 1uakt to R], fee payer P1 (exception only for burn-only txs) | included, refused in delivery by the lock | pass: code 1, h231: coins of akash1p8v4sd70qvvg737wthgeuafc7hh20uf5phg30x may only go to the recovery address |
| BU06 | X MsgExec(L0 MsgBurnACT to itself), X pays fee | refused at CheckTx by the lock | pass: CheckTx code 1: fee must be paid by one of the configured fee payers |
| BU07 | unlisted U MsgBurnACT 1000000uact own fee | succeeds | pass: code 0, h233; U before 90000970000 99994000000 after 90001965000 99993000000 |
| BU08 | full ACT path: L0 MsgSend of the BU01 payout 1000000uakt to R, fee payer P1 | succeeds | pass: code 0, h248; L0 to R 1000000uakt |
| DC01 | X signs MsgCloseDeployment of L0 dseq 2 (ID.Owner=L0), fee payer P1 | fails, not by the lock | pass: not signed: Error: tx intended signer does not match the given signer: X |
| DC02 | L0 MsgCloseDeployment dseq 2, own fee | refused at CheckTx by the lock | pass: CheckTx code 1: fee must be paid by one of the configured fee payers |
| DC03 | L0 MsgUpdateDeployment dseq 1, fee payer P1 | refused at CheckTx by the lock | pass: CheckTx code 1: message /akash.deployment.v1beta4.MsgUpdateDeployment is not allowed for a listed signer |
| DC04 | L0 MsgCreateDeployment dseq 20, fee payer P1 | refused at CheckTx by the lock | pass: CheckTx code 1: message /akash.deployment.v1beta4.MsgCreateDeployment is not allowed for a listed signer |
| DC05 | L0 MsgCloseGroup dseq 1 gseq 1, fee payer P1 | refused at CheckTx by the lock | pass: CheckTx code 1: message /akash.deployment.v1beta4.MsgCloseGroup is not allowed for a listed signer |
| DC06 | L0 MsgCloseDeployment dseq 2 (no lease), fee payer P1 | succeeds | pass: code 0, h252; escrow to L0 5000000uact |
| DC07 | L0 MsgCloseDeployment dseq 1 (active lease with PV), fee payer P1 | succeeds | pass: code 0, h254; escrow to PV 2370uact; escrow to PV 500000uakt; escrow to L0 4997630uact |
| DC08 | full deployment path: L0 MsgBurnACT of the refunded 9997630uact to itself, fee payer P1 | succeeds | pass: code 0, h259; L0 before 40000955000 49997997630 after 40010952630 49988000000 (uakt +9997630) |
| DC09 | full deployment path: L0 MsgSend 9997630uakt (refund converted) to R, fee payer P1 | succeeds | pass: code 0, h274; L0 to R 9997630uakt |
| DC10 | unlisted U MsgCloseDeployment dseq 2 | succeeds | pass: code 0, h275 |
| RW01 | L0 MsgWithdrawDelegatorReward val0 while withdraw address is X | refused at CheckTx by the lock | pass: CheckTx code 1: set the withdraw address to the wallet or its recovery address first |
| RW02 | L0 MsgUndelegate 1 AKT val0 while withdraw address is X | refused at CheckTx by the lock | pass: CheckTx code 1: set the withdraw address to the wallet or its recovery address first |
| RW03 | L0 MsgSetWithdrawAddress to itself | succeeds | pass: code 0, h278 |
| RW04 | L0 MsgUndelegate 1 AKT val0 (rewards auto-withdrawn) | succeeds | pass: code 0, h280; distribution to L0 12475648uakt |
| RW05 | L0 MsgSetWithdrawAddress to R | succeeds | pass: code 0, h282 |
| RW06 | L0 MsgWithdrawDelegatorReward val0 | succeeds | pass: code 0, h283; distribution to R 137882uakt |
| RW07 | X across RW01 to RW06 | receives nothing | pass: no transfer to X in RW04 or RW06; X balance after the run 1000000000001uakt and 1000000000000uact (genesis plus the 1uakt of B13) |
| RW08 | unlisted U MsgWithdrawDelegatorReward val0 (withdraw address T) | succeeds | pass: code 0, h285; distribution to T 12661532uakt |
| RW09 | unlisted U MsgUndelegate 1 AKT val0 (rewards auto-withdrawn to T) | succeeds | pass: code 0, h286; distribution to T 46000uakt |
| RW10 | unlisted validator val0 MsgWithdrawValidatorCommission plus rewards | succeeds | pass: code 0, h288; distribution to val0 operator 131814198uakt; distribution to val0 operator 17495213uakt |
| B02 | L0 MsgSend 1uakt to attacker X, fee payer P1 | refused at CheckTx by the lock | pass: CheckTx code 1: destination akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w is outside the allowed addresses for this message |
| B04 | L0 MsgSend 1uakt to R, own fee | refused at CheckTx by the lock | pass: CheckTx code 1: fee must be paid by one of the configured fee payers |
| B08 | unlisted U MsgSend with listed L0 as fee payer | refused at CheckTx by the lock | pass: CheckTx code 1: fee must be paid by one of the configured fee payers |
| B09 | L0 MsgSend to R, fee payer P1, zero fee | refused at CheckTx by the lock | pass: CheckTx code 1: fee must not be zero |
| D02 | L0 MsgSetWithdrawAddress to X | refused at CheckTx by the lock | pass: CheckTx code 1: destination akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w is outside the allowed addresses for this message |
| K03 | L0 MsgDelegate 1 AKT to val0 | refused at CheckTx by the lock | pass: CheckTx code 1: message /cosmos.staking.v1beta1.MsgDelegate is not allowed for a listed signer |
| K04 | L0 MsgBeginRedelegate 1 AKT val0 to val1 | refused at CheckTx by the lock | pass: CheckTx code 1: message /cosmos.staking.v1beta1.MsgBeginRedelegate is not allowed for a listed signer |
| A01 | X MsgExec(L0 MsgSend to X) with pre-activation grant | refused at CheckTx by the lock | pass: CheckTx code 1: destination akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w is outside the allowed addresses for this message |
| A04 | listed grantee L0 MsgExec(U MsgSend to X), fee payer P1 | refused at CheckTx by the lock | pass: CheckTx code 1: message /cosmos.authz.v1beta1.MsgExec is not allowed for a listed signer |
| AK02 | L0 escrow MsgAccountDeposit 1000000uact on dseq 1 | refused at CheckTx by the lock | pass: CheckTx code 1: message /akash.escrow.v1.MsgAccountDeposit is not allowed for a listed signer |
| AK12 | L0 BME MsgMintACT 10000000uakt | refused at CheckTx by the lock | pass: CheckTx code 1: message /akash.bme.v1.MsgMintACT is not allowed for a listed signer |
| B13 | unlisted U MsgSend to X, fee payer P1 | succeeds | pass: code 0, h296 |

ADOPT: tx at block 205, code 1 on the lock validators, L0 and R unchanged; node4 executed the send (L0 40000955000 to 39999955000) and stopped at 206 with `wrong Block.Header.AppHash`. node0 to node3 had identical app hashes at 199, 200, 205, 250 and 295 and kept producing blocks.

#### Chain C: abort, second attempt, expiry, replay (HC1 = 150, HC2 = 300)

node0 under cosmovisor v1.7.1 (same setup as chain A) armed with `add-upgrade recovery-lock <lock-c1> --upgrade-height 150`; node1 to node3 plain with `halt-height = 150`; node4 a full node on v2.2.0 throughout. A pre-signed L0 `MsgSend` of 1uakt to X with its own fee was broadcast as height 149 was reached and landed in block 150: a transaction the lock refuses and v2.2.0 executes.

| ID | Check | Result |
| --- | --- | --- |
| ABORT-STATE | the attempt at 150 stalls | pass. node0 halted, cosmovisor swapped it and lock-c1 executed 150 (app height 150, app hash `D6C225E4608969CFF5A07B559896C1EF7DA20FEBE9EAF961CFE551ADEA3E5F94`); node1 to node3 halted with app height 149; with 25 percent of the voting power the chain could not resume. |
| ABORT-DIFF | node0 must roll back | pass. node4 (v2.2.0) executed 150 to `D1A9AE727DB8E6D1D755B7C9BFF83F1CAD4F4D05A39A5D1C258AD61927E24063`, different from node0, because the lock refused the probe transaction in block 150. |
| abort | node0: stop, `akash rollback --home node0` (v2.2.0), `cosmovisor/current` back to `genesis`, `data/upgrade-info.json` removed, start; node1 to node3: start on v2.2.0 | `Rolled back state to height 149 and hash 8EA6113A82A287A7FD4B21574D74957EEFC4D521A9F4AFA5013D2F82B53EACEF`. The chain resumed on v2.2.0 at 19:31:50 UTC. |
| ABORT-RESUME | v2.2.0 everywhere | pass. The probe transaction `7FA4A652E47210709425854FAD81BD027D4B7940353F3D9770C0376B038D3225` in block 150 succeeded (code 0, 1uakt from L0 to X); node0 to node4 had the same app hash at 150, 151 and 154 (`D1A9AE727DB8E6D1D755B7C9BFF83F1CAD4F4D05A39A5D1C258AD61927E24063` at 151, the value node4 had computed before the abort). |
| second attempt | node0 re-armed with `add-upgrade recovery-lock-2 <lock-c2> --upgrade-height 300`, node1 to node3 `halt-height = 300`, swapped by hand | node0 switched to `upgrades/recovery-lock-2` by cosmovisor, the others by hand; lock-c2 logged `activation_height=300 expiry_time=1795203561 expiry_utc=2026-11-20T19:39:21Z`; the chain resumed at 19:35:34 UTC. |
| EXP-BEFORE | L0 `MsgSend` 1uakt to X, own fee, before expiry | pass. Refused at CheckTx: `destination akash162y697dk2lxr4k2fdgfmzf0yh00fgxnlzqfm6w is outside the allowed addresses for this message: recovery lock`. |
| EXP-AFTER | the same signed transaction once the block time passed 19:39:21 UTC | pass. Code 0, block 465 (tx `412DC8E537AD023C4B17176249A844FF2C2E0BCBD3EEBDA330AC531DC88C62A3`): the lock is off. |
| EXP-NODE4 | node4 (v2.2.0) after expiry | pass. Executed block 465 and stayed in sync: app hash at 466 `C45C8DCF4BB7266333436C8838A27876E9AA8C0BB138BD05DF3DD61B4E8299B8` on node0 and node4. |
| REPLAY-C | node5 with only lock-c2 syncs chain C from genesis (blocks 150 and 151 of the aborted attempt run with the lock not yet active, activation at 300, blocks after expiry) | pass. Reached 480 with no app hash error; app hashes equal to node0 at 150, 151, 300, 301, 466 and 480. |

#### Rerun on the final code: touched rows

Only the rows that the review cleanups can affect were rerun, with the same harness, network layout and test values: the adoption check, ACT burn, deployment close and its ACT burn, withdraw address and reward payout rows (the staking hooks run on every delegation change), IBC transfer, abort, expiry, replay. Binaries built from revision `70750568` with the tag `recoverylock_test`; base v2.2.0 rebuilt with the same sha256 `f842cb1727d3d63009f0e71120bdf11999f8e04d3f27b1178b33abec9349dde0`:

| Binary | activation_height | expiry_time | params sha256 | binary sha256 |
| --- | --- | --- | --- | --- |
| lock-fixb | 200 | 4102444800 | `c144d49606877891f884ab711a5f2f4210b5e8b4d5f60002062c69e5753a7669` | `dd10292bd1cd5f2e6cfd63f795efc83f7bb87dcdf25dc322cae18615a931e671` |
| lock-fixc1 | 150 | 4102444800 | `0fd25adf94f6b599f161c70b45b37d7c4541c65c3bf3511acf62429fff4b3587` | `642b569b8e2e251564db13617b029b6258b65d4fb1983252ed19dd53a4b47ea6` |
| lock-fixc2 | 300 | 1795213288 (2026-11-20T22:21:28Z) | `71498af711baa324a808608f45c1e21b24e0df465fc8cfad356ac6abf229296c` | `1886644d8662acb0c101dea0c32b44a8a3e0cf79f8fe6499ea430279610f5514` |

Chain B rerun (H = 200, node0 to node3 swapped by hand, node4 on v2.2.0; every lock node logged `recovery lock config` with `mode=test`): 73 of 73 rows pass (the chain B rows above plus I01 and I02).

| ID | Result on the final code |
| --- | --- |
| ADOPT | pass. P0 `MsgExec(L0 MsgSend 1000000uakt to R)` code 1 at h205 (`listed accounts may act only in transactions paid by a fee payer: recovery lock`), tx `1FDE7D660D1E0B6EC9E759A3A86954A7237BFFEEA2A3ADE165F84B4E1A37990F`; L0 and R unchanged on the lock nodes; node4 (v2.2.0) executed the send (L0 39999955000) and stopped at 205 with a wrong app hash. |
| BU01, BU01s | pass. `MsgBurnACT` to itself, fee payer P1, code 0 at h214, tx `5E3D70F215270A26C087E93D8649BFECF2ECA20E514D21F790CAD1D5E8F45540`; settled: uact -1000000, uakt +1000000. |
| BU02, BU03 | pass. To = X and To = R refused at CheckTx by BME validation (`owner and to addresses must match`, code 3). |
| BU04, BU06 | pass. Own fee, and through authz, refused at CheckTx by the lock (`fee must be paid by one of the configured fee payers`). |
| BU05 | pass. [burn, send to R] in one tx: included at h229, refused in delivery by the coin rule (`may only go to the recovery address`). |
| BU07, BU08 | pass. Unlisted burn unaffected (h231); the BU01 payout sent to R. |
| DC01 to DC10 | pass. Owner close refunds land in L0 (5000000uact without lease; 4997670uact back and 2330uact to PV with a lease); the refund burned (DC08, h254, tx `212CA54657831661B133CE6F4BCDD15CDC483BDD4AD38DFF00BDCCD51626C988`) and 9997670uakt sent to R (DC09). |
| RW01 to RW10 | pass. With the withdraw address X, withdraw and undelegate refused at CheckTx; after reset to the wallet, undelegate auto-withdrew rewards to L0 (RW04, h272); after setting R, rewards went to R; X unchanged (1000000000000uakt before and after); unlisted delegator and validator payouts as on v2.2.0. |
| I01 | pass. L0 `MsgTransfer`, fee payer P1, refused at CheckTx (`message /ibc.applications.transfer.v1.MsgTransfer is not allowed for a listed signer`). |
| I02 | pass. Unlisted U `MsgTransfer` included at h292 and failed on the missing counterparty (code 35), not on the lock. |

Chain C rerun (HC1 = 150 aborted, HC2 = 300 with a near expiry): ABORT-STATE, ABORT-DIFF, EXP-BEFORE, EXP-AFTER, EXP-NODE4 and REPLAY-C pass. The in-run ABORT-RESUME row recorded `not included after 20s` because the harness did not parse the broadcast output of the HC1 probe (same as in the first run); the probe tx `7FA4A652E47210709425854FAD81BD027D4B7940353F3D9770C0376B038D3225` is in block 150 with code 0, executed by v2.2.0 after the abort, and the app hashes at 150, 151 and 154 are equal on all five nodes (ABORT-RESUME2, pass). Expiry: the same signed L0 send to X was refused at CheckTx before 2026-11-20T22:21:28Z and included at h389 after it (tx `412DC8E537AD023C4B17176249A844FF2C2E0BCBD3EEBDA330AC531DC88C62A3`); node4 on v2.2.0 had the same app hash at 390 (`A885EA0069A9EEE96E1FF91D5ED086E450E06D752D1A5FF578FF01107BAD311F`). node5 with only lock-fixc2 replayed chain C from genesis to 400 with equal app hashes and no error.

### Copy of akashnet-2 mainnet state

A copy of real mainnet state with the lock active from its first block, run on a separate server (2026-11-20 21:21 to 21:31 UTC and 2026-11-21 08:23 to 08:40 UTC), every node process capped (CPU quota, IO weight, nice).

Method: Polkachu snapshot `akash_29598614.tar.lz4` (15,910,175,999 bytes, no checksum offered; handshake height 29598614, app hash `4530A4DA77C0388D2CBC101EAE8CED989940183E92451929E7FCA0C0B7E65A6A`). Akash's own `akash testnetify` (`cmd/akash/cmd/testnetify`, `app/testnet.go`) rewrote it in place into a one-validator chain `akash-statecopy-1` and funded the test accounts. testnetify ran with the lock binary itself, built with `activation_height` 29598615, so every block of the copy executed under the lock. A v2.2.0 start check on the same snapshot produced blocks without error first.

Lock build: release-shaped, built with the release tags `osusergo netgo muslc gcc` (no `recoverylock_test`; `ledger` dropped because its system library needs root on that server, which does not change consensus code). Fresh random keys for every wallet. List: the three test wallets L0, L1, L2 (recovery address R) plus 10,000 generated placeholder addresses, each with its own placeholder recovery address: 10,003 entries, `list_sha256` `d30c290093f75176e191849c430a4fb686b6258c353e8690f653c98cff0b691b`. 8 fee payers P0 to P7, `test_values` false, reference block 29588614 (2026-11-19T16:03:28Z), `unbonding_seconds` 1814400, `activation_time_estimate` 2026-11-20T09:00:00Z, `expiry_time` 1799398800 (2027-01-08T09:00:00Z). `params.json` sha256 `a2077d29c5a48614f65e0de145118f2fe1fd10333c0bce8f4fdce7dc941ad841`. Startup line on every start: `recovery lock config ... list_count=10003 mode=release fee_payers=8 recovery_count=10003`.

| Part | Binary | Blocks | Result |
| --- | --- | --- | --- |
| Run 1 | previous revision `bc356d69` (before the review cleanups), sha256 `671a35b26ab8ff787ee90d4545822a3f25dd46ccabee5318f5e3a64eca38f693` | 29598615 to 29598800 (186) | rows below; stopped at 29598800 (app hash `921D7BBFD19AA750CB27C15BC68FE00C9B613A7F3B99C42A2C6A93416309107D`) by an external guard on the shared server, not by the node |
| Restart 1 | same binary | 29598801 to 29598827 (27) | handshake at 29598800 with app hash `921D7BBFD19AA750CB27C15BC68FE00C9B613A7F3B99C42A2C6A93416309107D`, equal to the hash before the stop; no block replayed |
| Restart 2 | this commit's code (built from it with the same `listed.txt` and `params.json`), sha256 `f76a85f78d13d2b591a6af0e3afed30f1996d36b6833b2bd411a9e9e645985cf` | 29598828 to 29599227 (400) | handshake at 29598827 with the hash before the stop (`4FBC92FBE68282293200A4FFCDA3F2A2BD178657EBD3FD24C0F1785F716B8858`); rows marked F below |
| Restart 3 | this commit's code | 29599228 to 29599243 (16) | handshake at 29599227 with the hash before the stop (`348352711057241BDFB12B6241EA134174C046504BA649C932111A229BB2532A`) |

629 blocks under the lock in total, 0 panic, 0 consensus failure, 0 `Error after epoch end`, 0 `ERR` lines after the restarts. The review cleanups between the two binaries do not touch any transaction already in those blocks (the `MsgBurnACT` rule change concerns `To` = recovery address, which BME validation refuses anyway; the other cleanups do not change results), so the restart on the new binary continued the same state. Epochs on real state: `day` fired in the first block (epoch 243, start height 29598615); `hour` caught up the missed hours, the last at 29598829 after restart 2; `minute` fired in every block (348858 at 29599227); no epoch hook error. Active leases from the real state: 1438 at 29598695 (1437 real plus the test lease below), 1437 at 29599227 after the test lease was closed. BME status `mint_status_healthy` at the end. The oracle is fed in the copy by VAL after a passed gov proposal (copy only), so BME conversions work as on mainnet.

Run 1 rows (L0 signs unless stated, refusals at CheckTx with `recovery lock` unless stated):

| ID | Attempt | Result |
| --- | --- | --- |
| AT01, AT02 | send 1 AKT to X with fee payer P0, and with own fee | refused (`destination ... is outside the allowed addresses`) |
| AT03, AT05, AT06, AT17 | send to R with own fee; whole balance as own fee; X as fee payer; ACT burn with own fee | refused (`fee must be paid by one of the configured fee payers`) |
| AT04 | send to R, fee payer P0, zero fee | refused (`fee must not be zero`) |
| AT07, AT08, AT10, AT11, AT12, AT16, AT18 | delegate; redelegate; authz grant to X; IBC transfer on the real `channel-0`; deployment create; escrow deposit into another deployment; certificate publish | refused (`message ... is not allowed for a listed signer`) |
| AT09, AT14, AT15 | withdraw address X; multi-send to R and X; X `MsgExec` of an L0 send | refused (destination rule) |
| AT13 | `MsgBurnACT` with `to` = X | refused by BME validation (`owner and to addresses must match`) |
| BAL1 to BAL3 | balances of L0, L1, L2, X, R, P0 to P2, U before and after every refused attempt | identical |
| RC01 to RC04 | L0 500 AKT to R (P0), L1 whole balance to R (P1), L2 250 AKT to R (P2), L0 withdraw address R (P0) | code 0 at 29598710 to 29598713, txs `BF2D87590BC38C86ADEAACBDA3C2FB4179BF5FA017E29A2A80E939EAE4DFA293`, `9D40321E91E25CE6EF00294FC282BFF9ACD26F17A23BD4D0BC9D0ED4E40B2ECA`, `7C21BDD232C51C30F4B549CFABBFD638B6ABEE3A36E6F378382D1A0FE2A1F53E`, `7889EC7DA0E08D59F8D280C3D72BD7D1C5FFC75A5C7B6DFAF7703D8B396BC46D`; R gained exactly 100750000000uakt; L0, L1, L2 lost only the sent amounts; P0 and P1 paid 40000uakt (two fees each), P2 20000uakt |
| RC05, ACT1, RC06 | L0 burns 1 ACT to itself (P1), BME settles it, L0 sends the payout to R (P3) | code 0 at 29598714 (`68244211AAF87A6570020F1FB3E04AF02F4C6DA2B7F02B8DB40C5F192B6729ED`); L0 +1897173uakt at the next BME epoch; sent to R at 29598762 (`76D7B8A022393BB8EB85403DDBE73A2F6F61C3895A182481CD3E2E4E5CF1934F`) |
| UN01, UN02 | unlisted U sends to X and delegates, own fee | code 0 (lock does not affect unlisted accounts) |
| ES01 to ES05 | own provider withdraws its lease on U's deployment; U deposits into three real funded deployments; U closes its deployment | code 0; a deposit into a funded account does not settle it (`AccountDeposit` settles only overdrawn accounts) |

Restart 2 rows (this commit's code):

| ID | Attempt | Result |
| --- | --- | --- |
| AT01F, AT02F | L0 send 1 AKT to X, fee payer P0 and own fee | refused at CheckTx by the lock |
| AT13F | L0 `MsgBurnACT` with `to` = R, fee payer P1 | refused at CheckTx by BME validation (`owner and to addresses must match`) |
| BALF | balances before and after these three | identical |
| RC01F | L0 10 AKT to R, fee payer P0 | code 0 at 29598864, tx `A003AED46E60E6B6C915D54FEBAF43C606258B728B70FA12BE638E1DB5CC1E68`; R +10000000uakt, P0 -20000uakt fee, L0 -10000000uakt |
| ESOD1 | unlisted U deposits 1 ACT into the real overdrawn deployment `akash10945g6kvmfwplvzslr6xkp9qmjyfmyc03qhus5/26906391` (lease provider `akash12v6dhc8awlwhv438jjyw80eguhgtm735mfv3fx`, unsettled debt 8017uact) | code 0 at 29598866, tx `5FE92DD961A3154A588F4095243E0513C0B05973900C180CF6E558029CF292E3`; provider +8017uact, payment closed with withdrawn 10238017uact, the rest of the deposit refunded (U -8017uact net), account `overdrawn` to `closed` |
| ESOD2 | U deposits 1 ACT into the real overdrawn deployment `akash10x7439dz3pw4f4n3tmahm2esvcgrwjmagadcwl/24551095` (provider `akash14y0erczaqxw9p88qs7dd8mqdhn6c3j3c8emhku`, debt 10550266uact) | code 0 at 29598867, tx `386CBA902C9E4E47CF618F179D691F1BAFF0EA467B5DEE98BB772C3A31872D03`; the whole deposit paid the provider (+1000000uact), debt down to 9550266uact, account stays `overdrawn`: the partial settlement `x/escrow/keeper/keeper.go` does when the deposit is below the debt |

So real escrow settlement (debt payment to the provider, refund to the depositor, account close) works under the lock. Limits of the copy: one validator; the 10,000 placeholder list entries hold no funds, so real-state processes touched no listed account; the listed test wallets had no positions from before activation (undelegation, deployment close and the ACT burn with real positions are covered by local chains A and B).

### Performance

A generated list of 10,000 listed addresses (`sha256("recoverylock-bench-listed-<i>")[:20]`, each with its own generated recovery address, the 8 test fee payers). The test machine (14 cores) was shared with other work, load average about 7 during the runs; the numbers carry that noise.

Go benchmarks in `app/recoverylock_test.go`, `go test -tags recoverylock_test ./app/ -run xxx -bench RecoveryLock -benchtime 2s -count 6`:

| Benchmark | What | Six runs | Median |
| --- | --- | --- | --- |
| `BenchmarkRecoveryLockConfig10k` | startup parse and validation of the 10,000 address list (`MustConfig`) | 77.6 to 81.1 ms, 16.9 MB and 330491 allocations per load | 79.5 ms |
| `BenchmarkRecoveryLockAnte/disabled` | full ante handler of the app, ordinary bank send between unlisted accounts, committed configuration (no lock wired) | 190.8 to 206.9 us | 199.5 us |
| `BenchmarkRecoveryLockAnte/active-10k` | the same with the 10,000 address list active | 201.8 to 209.9 us, plus 28 allocations and about 1 KB per transaction | 207.0 us |

The lock adds about 7 us (about 4 percent) to the ante handler of an ordinary transaction, inside the spread of the runs. It consumes no gas: a one-off test (not committed) measured 56589 gas for the ante handler of that transaction both with the lock disabled and with the 10,000 address list active.

Local load run (`harness` script `perf.sh`, kept with the evidence): a single-validator chain with `timeout_commit` 1 s; 500 pre-signed ordinary bank sends (10 unlisted senders, 50 each) broadcast with `broadcast_tx_sync` from 10 parallel streams; node CPU time from `/proc/<pid>/stat` over the load, minus the idle CPU rate measured for 20 s before it; runs alternated v2.2.0 and the lock binary with the 10,000 address list active from height 2, three runs each:

| Binary | Start to height 1 | Load (500 txs) | Node CPU per tx | All CheckTx ok |
| --- | --- | --- | --- | --- |
| v2.2.0 | 1.56, 1.84, 1.70 s | 4.5, 4.9, 7.7 s | 1793, 1745, 2213 us | yes |
| lock, 10,000 listed | 1.96, 1.70, 1.83 s | 5.5, 4.8, 4.9 s | 1865, 1835, 1496 us | yes |

Per-transaction node CPU (CheckTx, block execution and commit together) varies from run to run by more than the difference between the binaries (medians 1793 us and 1835 us). The lock binary logged `recovery lock config ... list_count=10000 recovery_count=10000` at startup; start to the first block did not differ beyond run-to-run spread. Gas used per transaction was between 66028 and 92883 (the first transaction of each sender also stores its public key). The three v2.2.0 runs used the same set of values with different counts per value; in two of the three lock runs one transaction used 66028 and one 79179 gas, 3 below and 3 above the nearest v2.2.0 values. Not investigated further: the ante gas above is identical, and the lock's send restriction reads no store, so these single-transaction differences most likely come from the order of transactions in blocks (10 parallel streams), which differs between runs, not from a lock gas charge.

## Limits

Documented behaviour while the lock is active:

- After a node restart, CheckTx has no block time until the next commit and applies only the height condition. A node restarted after `expiry_time` refuses listed transactions in its own mempool until it commits the next block. Block execution (DeliverTx) uses the header time and is correct, so this affects only that node's mempool for one block.
- Authz `MsgExec` nested deeper than six levels (outer message at depth zero) is refused for every account, listed or not, during the window. Without the lock the SDK unpacks up to ten levels. Outside the window nothing changes.
- The decorator runs before signature verification and before `ConsumeGasForTxSize`. Anyone can send an unsigned transaction that names a listed signer; CheckTx then runs the lock checks (for contract exits a strict JSON parse, for withdrawals one withdraw address store read) before refusing it. The cost is bounded by the maximum transaction size and is similar to `ValidateBasic`. It does not affect consensus.
- A binary built with `-tags recoverylock_test` accepts test values and the known test addresses. The release defence is the startup log line `recovery lock config ... mode=release` (runbook step 6) and the published binary sha256 (runbook step 2): validators check both.

Not covered or not tested:

- Not tested: IBC with a real counterparty chain and relayer (the transfer refusal is at CheckTx and needs no channel), a contract dispatching messages as a listed wallet (the wrapped router held by CosmWasm is covered by the app tests; the same router ran live in every adoption check through authz), hook payouts triggered by a slashed redelegation or a removed validator (app tests only), state sync, Docker images (`make docker-image` and the docker part of `make release` need arm64 emulation on the build host), and publishing or signing a release.
- Existing deployments keep paying their providers from escrow until the recovery operator closes them; the escrow balance that comes back to the wallet shrinks until then.
- ACT leaves in two steps (burn, then send after the next BME epoch) and depends on a healthy oracle price and a BME status that allows ACT to AKT conversion.
- cw20 allowances and cw721 approvals granted before activation can be spent until the recovery operator removes them (W07 in chain A). The operator should remove rights first.
- Positions inside contracts (vaults, staking contracts) have no exit messages.
- Escrowed funds, outstanding IBC packets and BME or sink operations started before activation settle under their original rules.
- A fee payer transaction that wraps a listed wallet's message in authz `MsgExec` passes the ante filter and fails in delivery (fee charged). The operator signs listed messages directly; the adoption check uses this behaviour on purpose.
- Wallets missing from the list and value moved before the halt are not covered.
- The contract exit checks the JSON shape of the message, not the contract code.
