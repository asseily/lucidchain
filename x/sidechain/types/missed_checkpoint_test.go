package types_test

import (
	"testing"

	"cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/lucidpay/lucidchain/x/sidechain/types"
)

var (
	tNotarized = types.AssuranceTier_ASSURANCE_TIER_NOTARIZED
	tAttested  = types.AssuranceTier_ASSURANCE_TIER_ATTESTED

	actNone     = types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_NONE
	actPenalize = types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_PENALIZE
	actSuspend  = types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_SUSPEND
)

func policy(tier types.AssuranceTier, action types.MissedCheckpointAction, fraction string, suspendAfter uint32) types.MissedCheckpointPolicy {
	p := types.MissedCheckpointPolicy{
		Tier:                tier,
		Action:              action,
		GraceSeconds:        3600,
		SuspendAfterStrikes: suspendAfter,
	}
	if fraction != "" {
		p.PenaltyFraction = math.LegacyMustNewDecFromStr(fraction)
	}
	return p
}

func TestMaxMissedPenaltyFraction(t *testing.T) {
	require.True(t, types.MaxMissedPenaltyFraction().Equal(math.LegacyMustNewDecFromStr("0.25")))
}

func TestParamsValidate_MissedCheckpointPolicies(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *types.Params)
		valid  bool
	}{
		{"default params have no tier policies", func(p *types.Params) {}, true},
		{"example tier policies", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = types.DefaultMissedCheckpointPolicies()
		}, true},
		{"penalty fraction at the 25% cap", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{policy(tNotarized, actPenalize, "0.25", 1)}
		}, true},
		{"penalty fraction above 25%", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{policy(tNotarized, actPenalize, "0.250000000000000001", 1)}
		}, false},
		{"PENALIZE without suspend_after_strikes", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{policy(tNotarized, actPenalize, "0.01", 0)}
		}, false},
		{"PENALIZE with a zero fraction", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{policy(tNotarized, actPenalize, "0", 1)}
		}, false},
		{"PENALIZE with a nil fraction", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{policy(tNotarized, actPenalize, "", 1)}
		}, false},
		{"NONE with a non-zero fraction", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{policy(tNotarized, actNone, "0.01", 0)}
		}, false},
		{"SUSPEND with a non-zero fraction", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{policy(tNotarized, actSuspend, "0.01", 0)}
		}, false},
		{"NONE and SUSPEND with zero or nil fraction", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{
				policy(tNotarized, actNone, "", 0),
				policy(tAttested, actSuspend, "0", 0),
			}
		}, true},
		{"unspecified action", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{
				policy(tNotarized, types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_UNSPECIFIED, "0", 0),
			}
		}, false},
		{"unspecified tier", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{
				policy(types.AssuranceTier_ASSURANCE_TIER_UNSPECIFIED, actNone, "0", 0),
			}
		}, false},
		{"duplicate tier", func(p *types.Params) {
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{
				policy(tAttested, actNone, "0", 0),
				policy(tAttested, actSuspend, "0", 0),
			}
		}, false},
		{"grace above MaxPeriodSeconds", func(p *types.Params) {
			mp := policy(tNotarized, actNone, "0", 0)
			mp.GraceSeconds = types.MaxPeriodSeconds + 1
			p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{mp}
		}, false},
		{"max_block_gap_seconds above MaxPeriodSeconds", func(p *types.Params) {
			p.MaxBlockGapSeconds = types.MaxPeriodSeconds + 1
		}, false},
		{"max_block_gap_seconds 0 disables the guard", func(p *types.Params) {
			p.MaxBlockGapSeconds = 0
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := types.DefaultParams()
			tc.mutate(&p)
			err := p.Validate()
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestValidateMissedPolicyOverride(t *testing.T) {
	exempt := policy(types.AssuranceTier_ASSURANCE_TIER_UNSPECIFIED, actNone, "0", 0)
	require.NoError(t, types.ValidateMissedPolicyOverride(nil))
	require.NoError(t, types.ValidateMissedPolicyOverride(&exempt))

	bad := map[string]types.MissedCheckpointPolicy{
		"tier set":             policy(tNotarized, actNone, "0", 0),
		"fraction above 25%":   policy(types.AssuranceTier_ASSURANCE_TIER_UNSPECIFIED, actPenalize, "0.3", 1),
		"PENALIZE, no escal.":  policy(types.AssuranceTier_ASSURANCE_TIER_UNSPECIFIED, actPenalize, "0.01", 0),
		"NONE with a fraction": policy(types.AssuranceTier_ASSURANCE_TIER_UNSPECIFIED, actNone, "0.01", 0),
	}
	for name, mp := range bad {
		require.Error(t, types.ValidateMissedPolicyOverride(&mp), name)
	}
}

func TestEffectiveMissedPolicy(t *testing.T) {
	p := types.DefaultParams()
	p.MissedCheckpointGraceSeconds = 1234
	sc := types.Sidechain{Tier: tNotarized}

	// No tier entry: record strikes only, with the global grace.
	got := sc.EffectiveMissedPolicy(p)
	require.Equal(t, actNone, got.Action)
	require.Equal(t, uint64(1234), got.GraceSeconds)
	require.True(t, got.PenaltyFraction.IsZero())
	require.Zero(t, got.SuspendAfterStrikes)

	// Tier entry.
	tierPolicy := policy(tNotarized, actPenalize, "0.05", 3)
	p.MissedCheckpointPolicyByTier = []types.MissedCheckpointPolicy{policy(tAttested, actSuspend, "0", 0), tierPolicy}
	got = sc.EffectiveMissedPolicy(p)
	require.True(t, got.Equal(&tierPolicy))

	// Override wins.
	override := policy(types.AssuranceTier_ASSURANCE_TIER_UNSPECIFIED, actNone, "0", 0)
	sc.MissedPolicyOverride = &override
	got = sc.EffectiveMissedPolicy(p)
	require.True(t, got.Equal(&override))
}

func TestGenesisValidate_RejectsInvalidMissedPolicyOverride(t *testing.T) {
	bad := policy(tNotarized, actNone, "0", 0) // tier must be unset on an override
	gs := types.DefaultGenesis()
	gs.Sidechains = []types.Sidechain{{Id: "hospitality-platform-01", MissedPolicyOverride: &bad}}
	err := gs.Validate()
	require.ErrorIs(t, err, types.ErrInvalidGenesis)
	require.ErrorContains(t, err, "override")
}
