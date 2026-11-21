//go:build recoverylock_test

package recoverylock

import (
	"bytes"
	"testing"

	"github.com/cosmos/cosmos-sdk/codec/address"
	"github.com/stretchr/testify/require"
)

func TestUseTestConfig(t *testing.T) {
	ac := address.NewBech32Codec("akash")
	listed, paramsJSON := readTestData(t)
	restore := UseTestConfig(listed, paramsJSON)
	cfg := MustConfig(ac, nil, nil)
	require.NotNil(t, cfg)
	require.Len(t, cfg.recoveries, 3)
	restore()
	require.Nil(t, MustConfig(ac, nil, nil))
	bad := bytes.Replace(paramsJSON, []byte(`"list_count": 3`), []byte(`"list_count": 4`), 1)
	require.NotEqual(t, paramsJSON, bad)
	t.Cleanup(UseTestConfig(listed, bad))
	require.Panics(t, func() { MustConfig(ac, nil, nil) })
}
