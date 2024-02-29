package keeper

import (
	"testing"

	"github.com/omni-network/omni/halo/attest/testutil"
	"github.com/omni-network/omni/halo/attest/types"

	storetypes "cosmossdk.io/store/types"
	"github.com/cosmos/cosmos-sdk/runtime"
	sdktestutil "github.com/cosmos/cosmos-sdk/testutil"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

type mocks struct {
	skeeper *testutil.MockStakingKeeper
	voter   *testutil.MockVoter
	namer   *testutil.MockChainNamer
}

func mockDefaultExpectations(_ sdk.Context, m mocks) {}

func setupKeeper(t *testing.T, expectations ...func(sdk.Context, mocks)) (*Keeper, sdk.Context) {
	t.Helper()

	key := storetypes.NewKVStoreKey(types.StoreKey)
	storeSvc := runtime.NewKVStoreService(key)
	ctx := sdktestutil.DefaultContext(key, storetypes.NewTransientStoreKey("test_key"))
	codec := moduletestutil.MakeTestEncodingConfig().Codec

	// gomock initialization
	ctrl := gomock.NewController(t)
	m := mocks{
		skeeper: testutil.NewMockStakingKeeper(ctrl),
		voter:   testutil.NewMockVoter(ctrl),
		namer:   testutil.NewMockChainNamer(ctrl),
	}
	if len(expectations) == 0 {
		mockDefaultExpectations(ctx, m)
	} else {
		for _, exp := range expectations {
			exp(ctx, m)
		}
	}

	const voteWindow = 1
	k, err := New(codec, storeSvc, m.skeeper, m.namer.ChainName, voteWindow)
	require.NoError(t, err, "new keeper")

	return k, ctx
}

// dumpTables returns all the items in the atestation and signature tables as slices.
func dumpTables(t *testing.T, ctx sdk.Context, k *Keeper) ([]*Attestation, []*Signature) {
	t.Helper()
	var atts []*Attestation
	aitr, err := k.attTable.List(ctx, AttestationIdIndexKey{})
	require.NoError(t, err, "list attestations")
	defer aitr.Close()

	for aitr.Next() {
		a, err := aitr.Value()
		require.NoError(t, err, "signature iterator Value")
		atts = append(atts, a)
	}

	var sigs []*Signature
	sitr, err := k.sigTable.List(ctx, SignatureIdIndexKey{})
	require.NoError(t, err, "list signatures")
	defer sitr.Close()

	for sitr.Next() {
		s, err := sitr.Value()
		require.NoError(t, err, "signature iterator Value")
		sigs = append(sigs, s)
	}

	return atts, sigs
}