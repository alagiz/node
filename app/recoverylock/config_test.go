package recoverylock

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"

	"github.com/cosmos/cosmos-sdk/codec/address"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/stretchr/testify/require"
)

func readTestData(t *testing.T) (listed, paramsJSON []byte) {
	t.Helper()
	listed, err := os.ReadFile("testdata/listed.txt")
	require.NoError(t, err)
	paramsJSON, err = os.ReadFile("testdata/params.json")
	require.NoError(t, err)
	return listed, paramsJSON
}

func TestEmbeddedConfigDisabled(t *testing.T) {
	ac := address.NewBech32Codec("akash")
	for _, test := range []bool{false, true} {
		cfg, p, err := parseConfig(listedFile, paramsFile, ac, nil, test)
		require.NoError(t, err)
		require.Nil(t, cfg)
		require.False(t, p.Enabled)
	}
	require.Nil(t, MustConfig(ac, nil, nil))
	require.Nil(t, New(nil, nil, ac, nil, nil))
}

func TestDisabledConfigCarriesNoData(t *testing.T) {
	ac := address.NewBech32Codec("akash")
	listed, paramsJSON := readTestData(t)
	disabled := bytes.Replace(paramsJSON, []byte(`"enabled": true`), []byte(`"enabled": false`), 1)
	require.NotEqual(t, paramsJSON, disabled)
	_, _, err := parseConfig(listedFile, disabled, ac, nil, true)
	require.ErrorContains(t, err, "sets fields")
	_, _, err = parseConfig(listed, paramsFile, ac, nil, true)
	require.ErrorContains(t, err, "has entries")
	_, _, err = parseConfig(listedFile, []byte(`{"enabled": false, "list_count": 0, "fee_payers": []}`), ac, nil, true)
	require.ErrorContains(t, err, "sets fields")
}

func TestTestDataConfig(t *testing.T) {
	ac := address.NewBech32Codec("akash")
	listed, paramsJSON := readTestData(t)
	cfg, p, err := parseConfig(listed, paramsJSON, ac, nil, true)
	require.NoError(t, err)
	require.True(t, p.Enabled)
	require.True(t, p.TestValues)
	require.Len(t, cfg.recoveries, 3)
	require.Len(t, cfg.feePayers, 8)
	for i := 0; i < 3; i++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("recoverylock-test-listed-%d", i)))
		key := &secp256k1.PrivKey{Key: digest[:]}
		require.True(t, cfg.isListed(key.PubKey().Address()))
	}
	for i := 0; i < 8; i++ {
		digest := sha256.Sum256([]byte(fmt.Sprintf("recoverylock-test-fee-payer-%d", i)))
		key := &secp256k1.PrivKey{Key: digest[:]}
		require.True(t, cfg.isFeePayer(key.PubKey().Address()))
	}
	for wallet, destination := range cfg.recoveries {
		require.True(t, knownTestAddresses()[wallet])
		require.True(t, knownTestAddresses()[string(destination)])
	}
	// release rules refuse the test values
	_, _, err = parseConfig(listed, paramsJSON, ac, nil, false)
	require.ErrorContains(t, err, "test values")
}

func TestConfigValidation(t *testing.T) {
	ac := address.NewBech32Codec("akash")
	encode := func(i byte) string {
		s, err := ac.BytesToString(bytes.Repeat([]byte{i}, 20))
		require.NoError(t, err)
		return s
	}
	listed := []byte(encode(1) + "\n" + encode(2) + "\n")
	valid := func() params {
		return params{
			RecoveryAddresses:      map[string]string{encode(1): encode(3), encode(2): encode(4)},
			FeePayers:              []string{encode(5), encode(6), encode(7), encode(8), encode(9), encode(10), encode(11), encode(12)},
			ActivationHeight:       20_000_000,
			EstimateRefHeight:      19_990_000,
			EstimateRefTime:        1_700_000_000,
			ActivationTimeEstimate: 1_700_060_000,
			UnbondingSeconds:       MinReleaseUnbondingSeconds,
			ExpiryTime:             1_700_060_000 + 2*MinReleaseUnbondingSeconds + ExpiryMarginSeconds,
			ListCount:              2, ListSHA256: fmt.Sprintf("%x", sha256.Sum256(listed)),
		}
	}
	cfg, err := build(listed, valid(), ac, nil, false)
	require.NoError(t, err)
	require.True(t, cfg.isRecovery(bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{3}, 20)))
	require.False(t, cfg.isRecovery(bytes.Repeat([]byte{1}, 20), bytes.Repeat([]byte{4}, 20)))
	latest := valid()
	latest.ExpiryTime = latest.ActivationTimeEstimate + MaxReleaseWindowSeconds
	_, err = build(listed, latest, ac, nil, false)
	require.NoError(t, err)
	_, err = build(listed, valid(), ac, map[string]bool{encode(3): false}, false)
	require.ErrorContains(t, err, "module account")
	for _, tt := range []struct {
		name string
		edit func(*params)
	}{
		{"wrong hash", func(p *params) { p.ListSHA256 = "bad" }},
		{"wrong count", func(p *params) { p.ListCount++ }},
		{"missing recovery", func(p *params) { delete(p.RecoveryAddresses, encode(1)) }},
		{"recovery is listed", func(p *params) { p.RecoveryAddresses[encode(1)] = encode(2) }},
		{"payer is recovery", func(p *params) { p.FeePayers[0] = encode(3) }},
		{"payer is listed", func(p *params) { p.FeePayers[0] = encode(1) }},
		{"duplicate payer", func(p *params) { p.FeePayers[0] = p.FeePayers[1] }},
		{"too few release payers", func(p *params) { p.FeePayers = p.FeePayers[:7] }},
		{"test address", func(p *params) {
			_, paramsJSON := readTestData(t)
			fixtures, err := parseParams(paramsJSON)
			require.NoError(t, err)
			p.FeePayers[0] = fixtures.FeePayers[0]
		}},
		{"test values", func(p *params) { p.TestValues = true }},
		{"bad estimate", func(p *params) { p.ActivationHeight += 10_000_000 }},
		{"short unbonding time", func(p *params) { p.UnbondingSeconds-- }},
		{"early expiry", func(p *params) { p.ExpiryTime-- }},
		{"late expiry", func(p *params) { p.ExpiryTime = p.ActivationTimeEstimate + MaxReleaseWindowSeconds + 1 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := valid()
			tt.edit(&p)
			_, err := build(listed, p, ac, nil, false)
			require.Error(t, err)
		})
	}
}

func TestStrictParams(t *testing.T) {
	for _, json := range []string{
		`{"List_count":3}`, `{"unknown":3}`, `{"list_count":3,"list_count":3}`,
		`{"recovery_addresses":{"wallet":"destination","wallet":"other"}}`,
		`{"fee_payers":[7]}`, `{"list_count":3} {}`,
	} {
		_, err := parseParams([]byte(json))
		require.Error(t, err, "%s", json)
	}
}
