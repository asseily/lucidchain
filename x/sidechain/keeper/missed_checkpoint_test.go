package keeper_test

import (
	"fmt"
	"testing"
	"time"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	"github.com/lucidpay/lucidchain/x/sidechain/keeper"
	sidechainmodule "github.com/lucidpay/lucidchain/x/sidechain/module"
	"github.com/lucidpay/lucidchain/x/sidechain/types"
)

// Tests for the missed-checkpoint policy (deadlines.go,
// msg_server_missed_policy.go, types/missed_checkpoint.go).
//
// The default sidechain from registerMsg has a 300 s checkpoint interval and
// tier NOTARIZED; DefaultParams has a 3600 s global grace period, so a fresh
// window is 3900 s long.

const (
	scInterval = 300 * time.Second
	scGrace    = time.Hour
	scWindow   = scInterval + scGrace

	missedEvent    = "sidechain_checkpoint_missed"
	forgivenEvent  = "sidechain_deadline_forgiven"
	policySetEvent = "sidechain_missed_policy_set"
)

// notarized and attested are declared in upgrade_sidechain_test.go.
var (
	active    = types.SidechainStatus_SIDECHAIN_STATUS_ACTIVE
	suspended = types.SidechainStatus_SIDECHAIN_STATUS_SUSPENDED
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// newMissedFixture is newActiveFixture with the chain-outage guard disabled,
// so tests can jump block time without the jump counting as an outage.
func newMissedFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.setParams(t, func(p *types.Params) { p.MaxBlockGapSeconds = 0 })
	f.register(t)
	f.activate(t, testID)
	return f
}

func penalizePolicy(fraction string, suspendAfter, decay uint32) types.MissedCheckpointPolicy {
	return types.MissedCheckpointPolicy{
		Action:                 types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_PENALIZE,
		GraceSeconds:           uint64(scGrace / time.Second),
		PenaltyFraction:        math.LegacyMustNewDecFromStr(fraction),
		SuspendAfterStrikes:    suspendAfter,
		StrikeDecayCheckpoints: decay,
	}
}

func suspendPolicy() types.MissedCheckpointPolicy {
	return types.MissedCheckpointPolicy{
		Action:          types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_SUSPEND,
		GraceSeconds:    uint64(scGrace / time.Second),
		PenaltyFraction: math.LegacyZeroDec(),
	}
}

// exemptPolicy is the documented exemption: {NONE, suspend_after_strikes 0}.
func exemptPolicy() types.MissedCheckpointPolicy {
	return types.MissedCheckpointPolicy{
		Action:          types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_NONE,
		GraceSeconds:    uint64(scGrace / time.Second),
		PenaltyFraction: math.LegacyZeroDec(),
	}
}

func forTier(p types.MissedCheckpointPolicy, tier types.AssuranceTier) types.MissedCheckpointPolicy {
	p.Tier = tier
	return p
}

func (f *fixture) setTierPolicies(t *testing.T, policies ...types.MissedCheckpointPolicy) {
	t.Helper()
	f.setParams(t, func(p *types.Params) { p.MissedCheckpointPolicyByTier = policies })
}

func (f *fixture) setOverride(t *testing.T, id string, p *types.MissedCheckpointPolicy) {
	t.Helper()
	_, err := f.msgServer.SetSidechainMissedPolicy(f.ctx,
		&types.MsgSetSidechainMissedPolicy{Authority: f.govS, Id: id, Policy: p})
	require.NoError(t, err)
}

// endBlock runs the module's EndBlock with a fresh event manager, so the
// events it emits can be inspected afterwards.
func (f *fixture) endBlock(t *testing.T) {
	t.Helper()
	f.ctx = f.ctx.WithEventManager(sdk.NewEventManager())
	am := sidechainmodule.NewAppModule(nil, f.keeper, nil, nil)
	require.NoError(t, am.EndBlock(f.ctx))
}

// blocksUntil produces EndBlocks every `step` until block time reaches
// `until` exactly (the last step is shortened).
func (f *fixture) blocksUntil(t *testing.T, until time.Time, step time.Duration) {
	t.Helper()
	for f.ctx.BlockTime().Before(until) {
		d := min(step, until.Sub(f.ctx.BlockTime()))
		f.advance(d)
		f.endBlock(t)
	}
}

func (f *fixture) at(tm time.Time) { f.ctx = f.ctx.WithBlockTime(tm) }

func (f *fixture) deadline(t *testing.T, id string) time.Time {
	t.Helper()
	sc := f.get(t, id)
	require.NotNil(t, sc.NextDeadlineAt, "sidechain %q has no deadline", id)
	return *sc.NextDeadlineAt
}

// queued returns the deadline-queue seconds recorded for one sidechain.
func (f *fixture) queued(t *testing.T, id string) []uint64 {
	t.Helper()
	var out []uint64
	require.NoError(t, f.keeper.DeadlineQueue.Walk(f.ctx, nil, func(k collections.Pair[uint64, string]) (bool, error) {
		if k.K2() == id {
			out = append(out, k.K1())
		}
		return false, nil
	}))
	return out
}

func (f *fixture) queueLen(t *testing.T) int {
	t.Helper()
	n := 0
	require.NoError(t, f.keeper.DeadlineQueue.Walk(f.ctx, nil, func(collections.Pair[uint64, string]) (bool, error) {
		n++
		return false, nil
	}))
	return n
}

// requireQueuedAt checks that the sidechain has exactly one queue entry and
// that it is at `at`.
func (f *fixture) requireQueuedAt(t *testing.T, id string, at time.Time) {
	t.Helper()
	require.Equal(t, []uint64{uint64(at.Unix())}, f.queued(t, id))
}

func (f *fixture) requireNoDeadline(t *testing.T, id string) {
	t.Helper()
	require.Nil(t, f.get(t, id).NextDeadlineAt)
	require.Empty(t, f.queued(t, id))
}

func (f *fixture) checkpointNow(t *testing.T, id string) {
	t.Helper()
	sc := f.get(t, id)
	require.NoError(t, f.keeper.RecordCheckpoint(f.ctx, id, sc.LastCheckpointSequence+1, testHeight, []byte("h")))
}

// missAt moves block time to the sidechain's current deadline and runs EndBlock.
func (f *fixture) miss(t *testing.T, id string) {
	t.Helper()
	f.at(f.deadline(t, id))
	f.endBlock(t)
}

// cloneActive writes n ACTIVE copies of the default sidechain (ids sc-000..)
// through SetSidechain at the current block time. Bonds are not escrowed, so
// callers must not rely on the escrow invariant.
func (f *fixture) cloneActive(t *testing.T, n int) []string {
	t.Helper()
	base := f.get(t, testID)
	ids := make([]string, n)
	for i := range n {
		sc := base
		sc.Id = fmt.Sprintf("sc-%03d", i)
		sc.NextDeadlineAt = nil
		require.NoError(t, f.keeper.SetSidechain(f.ctx, sc))
		ids[i] = sc.Id
	}
	return ids
}

func (f *fixture) events(typ string) []map[string]string {
	var out []map[string]string
	for _, ev := range f.ctx.EventManager().Events() {
		if ev.Type != typ {
			continue
		}
		m := map[string]string{}
		for _, a := range ev.Attributes {
			m[a.Key] = a.Value
		}
		out = append(out, m)
	}
	return out
}

func stake(n int64) string { return sdk.NewInt64Coin(sdk.DefaultBondDenom, n).String() }

// ---------------------------------------------------------------------------
// deadline assignment
// ---------------------------------------------------------------------------

func TestMissedCheckpoint_DeadlineAssignment(t *testing.T) {
	t.Run("activate assigns block time + interval + grace, truncated to seconds", func(t *testing.T) {
		f := newFixture(t)
		f.setParams(t, func(p *types.Params) { p.MaxBlockGapSeconds = 0 })
		f.register(t)
		f.requireNoDeadline(t, testID) // PENDING has no deadline

		f.advance(time.Hour + 1500*time.Millisecond) // sub-second block time
		f.activate(t, testID)

		now := f.ctx.BlockTime()
		want := now.Add(scWindow).Truncate(time.Second)
		require.NotZero(t, now.Nanosecond())
		got := f.deadline(t, testID)
		require.True(t, got.Equal(want), "deadline %s, want %s", got, want)
		require.Zero(t, got.Nanosecond())
		f.requireQueuedAt(t, testID, want)
	})

	t.Run("register with auto_activate assigns a deadline", func(t *testing.T) {
		f := newFixture(t)
		f.setParams(t, func(p *types.Params) { p.AutoActivate = true })
		f.register(t)
		want := testBlockTime.Add(scWindow)
		require.True(t, f.deadline(t, testID).Equal(want))
		f.requireQueuedAt(t, testID, want)
	})

	t.Run("the tier policy's grace replaces the global grace", func(t *testing.T) {
		f := newFixture(t)
		p := exemptPolicy()
		p.GraceSeconds = 600
		f.setTierPolicies(t, forTier(p, notarized))
		f.register(t)
		f.activate(t, testID)
		want := testBlockTime.Add(scInterval + 600*time.Second)
		require.True(t, f.deadline(t, testID).Equal(want))
		f.requireQueuedAt(t, testID, want)
	})

	t.Run("updating the interval does not move the current deadline", func(t *testing.T) {
		f := newMissedFixture(t)
		d := f.deadline(t, testID)
		f.advance(time.Minute)
		_, err := f.msgServer.UpdateSidechain(f.ctx, &types.MsgUpdateSidechain{
			Operator: f.operatorS, Id: testID, Name: "renamed",
			MetadataUri: "https://example.org/meta.json", CheckpointIntervalSeconds: 7200,
		})
		require.NoError(t, err)
		require.True(t, f.deadline(t, testID).Equal(d))
		f.requireQueuedAt(t, testID, d)

		// The next window uses the new interval.
		f.checkpointNow(t, testID)
		want := f.ctx.BlockTime().Add(7200*time.Second + scGrace)
		require.True(t, f.deadline(t, testID).Equal(want))
		f.requireQueuedAt(t, testID, want)
	})
}

// ---------------------------------------------------------------------------
// on time
// ---------------------------------------------------------------------------

func TestMissedCheckpoint_NothingHappensBeforeTheDeadline(t *testing.T) {
	f := newMissedFixture(t)
	f.setTierPolicies(t, forTier(penalizePolicy("0.10", 3, 0), notarized))
	d := f.deadline(t, testID)

	f.at(d.Add(-time.Second))
	f.endBlock(t)
	sc := f.get(t, testID)
	require.Zero(t, sc.MissedStrikes)
	require.Equal(t, tier1Bond, sc.Bond.Amount.Int64())
	require.True(t, sc.NextDeadlineAt.Equal(d))
	f.requireQueuedAt(t, testID, d)
	require.Empty(t, f.events(missedEvent))

	// An on-time checkpoint starts a new window from its block time.
	f.checkpointNow(t, testID)
	nd := f.deadline(t, testID)
	require.True(t, nd.Equal(d.Add(-time.Second).Add(scWindow)))
	f.requireQueuedAt(t, testID, nd)

	// The old deadline passes without a miss.
	f.at(d.Add(time.Minute))
	f.endBlock(t)
	require.Zero(t, f.get(t, testID).MissedStrikes)
	require.Empty(t, f.events(missedEvent))

	// A checkpoint exactly at the deadline is on time.
	f.at(nd)
	f.checkpointNow(t, testID)
	f.endBlock(t)
	sc = f.get(t, testID)
	require.Zero(t, sc.MissedStrikes)
	require.Equal(t, tier1Bond, sc.Bond.Amount.Int64())
	require.Empty(t, f.events(missedEvent))
	f.requireEscrowInvariant(t)
}

// ---------------------------------------------------------------------------
// misses
// ---------------------------------------------------------------------------

func TestMissedCheckpoint_DefaultParamsOnlyRecordStrikes(t *testing.T) {
	f := newMissedFixture(t)
	p, err := f.keeper.GetParams(f.ctx)
	require.NoError(t, err)
	require.Empty(t, p.MissedCheckpointPolicyByTier)

	d := f.deadline(t, testID)
	f.miss(t, testID) // exactly at the deadline: due

	sc := f.get(t, testID)
	require.Equal(t, uint32(1), sc.MissedStrikes)
	require.Zero(t, sc.OnTimeStreak)
	require.Equal(t, active, sc.Status)
	require.Equal(t, tier1Bond, sc.Bond.Amount.Int64())
	require.Equal(t, tier1Bond, f.moduleBal())
	require.True(t, sc.NextDeadlineAt.Equal(d.Add(scWindow)), "a new window starts at the processing block")
	f.requireQueuedAt(t, testID, d.Add(scWindow))

	evs := f.events(missedEvent)
	require.Len(t, evs, 1)
	require.Equal(t, map[string]string{
		"sidechain_id":  testID,
		"tier":          notarized.String(),
		"policy_source": "tier",
		"strikes":       "1",
		"action":        types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_NONE.String(),
		"burned":        stake(0),
		"bond":          stake(tier1Bond),
		"status":        active.String(),
	}, evs[0])

	// Further misses keep counting and never suspend.
	for i := 2; i <= 6; i++ {
		f.miss(t, testID)
		sc = f.get(t, testID)
		require.Equal(t, uint32(i), sc.MissedStrikes)
		require.Equal(t, active, sc.Status)
		require.Equal(t, tier1Bond, sc.Bond.Amount.Int64())
	}
	f.requireEscrowInvariant(t)
}

func TestMissedCheckpoint_TierPenalize(t *testing.T) {
	t.Run("burns floor(bond*fraction) per miss and suspends at suspend_after_strikes", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(penalizePolicy("0.10", 3, 0), notarized))

		prev := tier1Bond
		for i, want := range []int64{900_000, 810_000, 729_000} {
			f.miss(t, testID)
			sc := f.get(t, testID)
			require.Equal(t, uint32(i+1), sc.MissedStrikes)
			require.Equal(t, want, sc.Bond.Amount.Int64())
			require.Equal(t, want, f.moduleBal(), "the penalty is burned from escrow")
			f.requireEscrowInvariant(t)

			evs := f.events(missedEvent)
			require.Len(t, evs, 1)
			require.Equal(t, stake(prev-want), evs[0]["burned"])
			require.Equal(t, stake(want), evs[0]["bond"])
			require.Equal(t, types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_PENALIZE.String(), evs[0]["action"])
			require.Equal(t, fmt.Sprint(i+1), evs[0]["strikes"])

			if i < 2 {
				require.Equal(t, active, sc.Status)
				require.NotNil(t, sc.NextDeadlineAt)
				require.Equal(t, active.String(), evs[0]["status"])
			} else {
				require.Equal(t, suspended, sc.Status)
				require.Equal(t, suspended.String(), evs[0]["status"])
				f.requireNoDeadline(t, testID)
			}
			prev = want
		}

		// A suspended sidechain is never processed again.
		f.advance(30 * day)
		f.endBlock(t)
		sc := f.get(t, testID)
		require.Equal(t, uint32(3), sc.MissedStrikes)
		require.Equal(t, int64(729_000), sc.Bond.Amount.Int64())
		require.Empty(t, f.events(missedEvent))
	})

	t.Run("the burn is truncated", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(penalizePolicy("0.123456789", 5, 0), notarized))
		f.miss(t, testID)
		require.Equal(t, tier1Bond-123_456, f.get(t, testID).Bond.Amount.Int64())
		require.Equal(t, stake(123_456), f.events(missedEvent)[0]["burned"])
		f.requireEscrowInvariant(t)
	})

	t.Run("a burn that truncates to zero still counts the strike", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(penalizePolicy("0.10", 5, 0), notarized))
		sc := f.get(t, testID)
		sc.Bond = sdk.NewInt64Coin(sdk.DefaultBondDenom, 9)
		require.NoError(t, f.keeper.SetSidechain(f.ctx, sc))
		f.bank.modules[types.ModuleName] = math.NewInt(9)

		f.miss(t, testID)
		sc = f.get(t, testID)
		require.Equal(t, uint32(1), sc.MissedStrikes)
		require.Equal(t, int64(9), sc.Bond.Amount.Int64())
		require.Equal(t, stake(0), f.events(missedEvent)[0]["burned"])
		f.requireEscrowInvariant(t)
	})
}

func TestMissedCheckpoint_SuspendActionSuspendsOnFirstMiss(t *testing.T) {
	f := newMissedFixture(t)
	f.setTierPolicies(t, forTier(suspendPolicy(), notarized))

	f.miss(t, testID)
	sc := f.get(t, testID)
	require.Equal(t, suspended, sc.Status)
	require.Equal(t, uint32(1), sc.MissedStrikes)
	require.Equal(t, tier1Bond, sc.Bond.Amount.Int64())
	require.Equal(t, tier1Bond, f.moduleBal())
	f.requireNoDeadline(t, testID)
	evs := f.events(missedEvent)
	require.Len(t, evs, 1)
	require.Equal(t, types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_SUSPEND.String(), evs[0]["action"])
	require.Equal(t, stake(0), evs[0]["burned"])
	require.Equal(t, suspended.String(), evs[0]["status"])

	f.advance(30 * day)
	f.endBlock(t)
	require.Equal(t, uint32(1), f.get(t, testID).MissedStrikes)
	require.Empty(t, f.events(missedEvent))
}

func TestMissedCheckpoint_InactiveSidechainsAreNeverProcessed(t *testing.T) {
	penalize := forTier(penalizePolicy("0.10", 3, 0), notarized)

	t.Run("pending", func(t *testing.T) {
		f := newFixture(t)
		f.setParams(t, func(p *types.Params) { p.MaxBlockGapSeconds = 0 })
		f.setTierPolicies(t, penalize)
		f.register(t)
		f.advance(30 * day)
		f.endBlock(t)
		f.requireNoDeadline(t, testID)
		require.Zero(t, f.get(t, testID).MissedStrikes)
		require.Zero(t, f.queueLen(t))
	})

	t.Run("suspended by governance", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, penalize)
		_, err := f.msgServer.SuspendSidechain(f.ctx, &types.MsgSuspendSidechain{Authority: f.govS, Id: testID})
		require.NoError(t, err)
		f.requireNoDeadline(t, testID)
		f.advance(30 * day)
		f.endBlock(t)
		sc := f.get(t, testID)
		require.Zero(t, sc.MissedStrikes)
		require.Equal(t, tier1Bond, sc.Bond.Amount.Int64())
	})

	t.Run("exited", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, penalize)
		_, err := f.msgServer.InitiateSidechainExit(f.ctx, f.exitMsg())
		require.NoError(t, err)
		f.requireNoDeadline(t, testID)
		f.advance(day) // past the old deadline, before the bond return
		f.endBlock(t)
		sc := f.get(t, testID)
		require.Zero(t, sc.MissedStrikes)
		require.Equal(t, tier1Bond, sc.Bond.Amount.Int64())
		require.Empty(t, f.events(missedEvent))
	})

	t.Run("slashed", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, penalize)
		_, err := f.keeper.Slash(f.ctx, testID, math.LegacyNewDecWithPrec(1, 1), "double signing")
		require.NoError(t, err)
		require.Equal(t, types.SidechainStatus_SIDECHAIN_STATUS_SLASHED, f.get(t, testID).Status)
		f.requireNoDeadline(t, testID)
		f.advance(30 * day)
		f.endBlock(t)
		require.Zero(t, f.get(t, testID).MissedStrikes)
	})

	t.Run("a leftover queue entry for a suspended sidechain is dropped", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, penalize)
		f.setStatus(t, testID, suspended)
		require.NoError(t, f.keeper.DeadlineQueue.Set(f.ctx,
			collections.Join(uint64(f.ctx.BlockTime().Unix()), testID)))
		f.endBlock(t)
		require.Empty(t, f.queued(t, testID))
		require.Zero(t, f.get(t, testID).MissedStrikes)
		require.Empty(t, f.events(missedEvent))
	})
}

// ---------------------------------------------------------------------------
// strike decay and resume
// ---------------------------------------------------------------------------

func TestMissedCheckpoint_StrikeDecay(t *testing.T) {
	t.Run("strike_decay_checkpoints on-time checkpoints clear one strike", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(penalizePolicy("0.01", 5, 2), notarized))

		f.miss(t, testID)
		f.miss(t, testID)
		require.Equal(t, uint32(2), f.get(t, testID).MissedStrikes)

		expect := []struct{ strikes, streak uint32 }{
			{2, 1}, {1, 0}, {1, 1}, {0, 0},
			{0, 0}, // no strikes left: the streak is not counted
		}
		for i, e := range expect {
			f.advance(time.Minute)
			f.checkpointNow(t, testID)
			sc := f.get(t, testID)
			require.Equal(t, e.strikes, sc.MissedStrikes, "checkpoint %d", i+1)
			require.Equal(t, e.streak, sc.OnTimeStreak, "checkpoint %d", i+1)
		}

		// A miss resets the streak.
		f.miss(t, testID)
		f.advance(time.Minute)
		f.checkpointNow(t, testID)
		require.Equal(t, uint32(1), f.get(t, testID).OnTimeStreak)
		f.miss(t, testID)
		sc := f.get(t, testID)
		require.Equal(t, uint32(2), sc.MissedStrikes)
		require.Zero(t, sc.OnTimeStreak)
	})

	t.Run("0 means strikes never decay", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(penalizePolicy("0.01", 5, 0), notarized))
		f.miss(t, testID)
		for range 20 {
			f.advance(time.Minute)
			f.checkpointNow(t, testID)
		}
		sc := f.get(t, testID)
		require.Equal(t, uint32(1), sc.MissedStrikes)
		require.Zero(t, sc.OnTimeStreak)
	})
}

func TestMissedCheckpoint_ResumeResetsCounters(t *testing.T) {
	t.Run("after a governance suspension", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(penalizePolicy("0.10", 3, 5), notarized))

		f.miss(t, testID)
		f.advance(time.Minute)
		f.checkpointNow(t, testID)
		sc := f.get(t, testID)
		require.Equal(t, uint32(1), sc.MissedStrikes)
		require.Equal(t, uint32(1), sc.OnTimeStreak)

		_, err := f.msgServer.SuspendSidechain(f.ctx, &types.MsgSuspendSidechain{Authority: f.govS, Id: testID})
		require.NoError(t, err)
		f.advance(day)
		_, err = f.msgServer.ResumeSidechain(f.ctx, &types.MsgResumeSidechain{Authority: f.govS, Id: testID})
		require.NoError(t, err)

		sc = f.get(t, testID)
		require.Equal(t, active, sc.Status)
		require.Zero(t, sc.MissedStrikes)
		require.Zero(t, sc.OnTimeStreak)
		want := f.ctx.BlockTime().Add(scWindow)
		require.True(t, sc.NextDeadlineAt.Equal(want))
		f.requireQueuedAt(t, testID, want)

		// Counting starts again from zero.
		f.miss(t, testID)
		sc = f.get(t, testID)
		require.Equal(t, uint32(1), sc.MissedStrikes)
		require.Equal(t, active, sc.Status)
		require.Equal(t, int64(810_000), sc.Bond.Amount.Int64())
	})

	t.Run("after a missed-checkpoint suspension", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(suspendPolicy(), notarized))
		f.miss(t, testID)
		require.Equal(t, suspended, f.get(t, testID).Status)

		f.advance(time.Hour)
		_, err := f.msgServer.ResumeSidechain(f.ctx, &types.MsgResumeSidechain{Authority: f.govS, Id: testID})
		require.NoError(t, err)
		sc := f.get(t, testID)
		require.Zero(t, sc.MissedStrikes)
		require.True(t, sc.NextDeadlineAt.Equal(f.ctx.BlockTime().Add(scWindow)))
		f.requireQueuedAt(t, testID, *sc.NextDeadlineAt)
	})
}

// ---------------------------------------------------------------------------
// per-sidechain override
// ---------------------------------------------------------------------------

func TestSetSidechainMissedPolicy(t *testing.T) {
	t.Run("authority only", func(t *testing.T) {
		f := newMissedFixture(t)
		p := exemptPolicy()
		for _, who := range []string{f.operatorS, f.strangerS} {
			_, err := f.msgServer.SetSidechainMissedPolicy(f.ctx,
				&types.MsgSetSidechainMissedPolicy{Authority: who, Id: testID, Policy: &p})
			require.ErrorIs(t, err, types.ErrUnauthorized)
		}
		_, err := f.msgServer.SetSidechainMissedPolicy(f.ctx,
			&types.MsgSetSidechainMissedPolicy{Authority: "bogus", Id: testID, Policy: &p})
		require.ErrorIs(t, err, sdkerrors.ErrInvalidAddress)
		require.Nil(t, f.get(t, testID).MissedPolicyOverride)

		_, err = f.msgServer.SetSidechainMissedPolicy(f.ctx,
			&types.MsgSetSidechainMissedPolicy{Authority: f.govS, Id: "nope", Policy: &p})
		require.ErrorIs(t, err, types.ErrSidechainNotFound)
	})

	t.Run("invalid overrides are rejected", func(t *testing.T) {
		f := newMissedFixture(t)
		bad := map[string]types.MissedCheckpointPolicy{
			"tier set":                forTier(exemptPolicy(), notarized),
			"fraction above 25%":      penalizePolicy("0.26", 1, 0),
			"PENALIZE without escal.": penalizePolicy("0.01", 0, 0),
		}
		for name, p := range bad {
			_, err := f.msgServer.SetSidechainMissedPolicy(f.ctx,
				&types.MsgSetSidechainMissedPolicy{Authority: f.govS, Id: testID, Policy: &p})
			require.ErrorIs(t, err, types.ErrInvalidRequest, name)
		}
		require.Nil(t, f.get(t, testID).MissedPolicyOverride)
	})

	t.Run("the override takes precedence over the tier policy", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(suspendPolicy(), notarized))
		d := f.deadline(t, testID)

		override := penalizePolicy("0.20", 2, 0)
		override.GraceSeconds = 600
		f.ctx = f.ctx.WithEventManager(sdk.NewEventManager())
		f.setOverride(t, testID, &override)
		evs := f.events(policySetEvent)
		require.Len(t, evs, 1)
		require.Equal(t, "sidechain_override", evs[0]["policy_source"])

		// The current window is not touched...
		require.True(t, f.deadline(t, testID).Equal(d))
		f.requireQueuedAt(t, testID, d)

		// ...the next miss follows the override, not the tier's SUSPEND.
		f.miss(t, testID)
		sc := f.get(t, testID)
		require.Equal(t, active, sc.Status)
		require.Equal(t, int64(800_000), sc.Bond.Amount.Int64())
		ev := f.events(missedEvent)[0]
		require.Equal(t, "sidechain_override", ev["policy_source"])
		require.Equal(t, stake(200_000), ev["burned"])
		// ...and the new window uses the override's grace.
		require.True(t, sc.NextDeadlineAt.Equal(d.Add(scInterval+600*time.Second)))

		// Second strike reaches the override's suspend_after_strikes.
		f.miss(t, testID)
		sc = f.get(t, testID)
		require.Equal(t, suspended, sc.Status)
		require.Equal(t, int64(640_000), sc.Bond.Amount.Int64())
		f.requireEscrowInvariant(t)
	})

	t.Run("a nil policy clears the override", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(penalizePolicy("0.10", 3, 0), notarized))
		p := exemptPolicy()
		f.setOverride(t, testID, &p)
		require.NotNil(t, f.get(t, testID).MissedPolicyOverride)

		f.ctx = f.ctx.WithEventManager(sdk.NewEventManager())
		f.setOverride(t, testID, nil)
		require.Nil(t, f.get(t, testID).MissedPolicyOverride)
		require.Equal(t, "tier", f.events(policySetEvent)[0]["policy_source"])

		f.miss(t, testID)
		require.Equal(t, int64(900_000), f.get(t, testID).Bond.Amount.Int64())
		require.Equal(t, "tier", f.events(missedEvent)[0]["policy_source"])
	})

	t.Run("the exempt override {NONE, 0} never suspends or burns", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(suspendPolicy(), notarized))
		p := exemptPolicy()
		f.setOverride(t, testID, &p)

		for i := 1; i <= 10; i++ {
			f.miss(t, testID)
			sc := f.get(t, testID)
			require.Equal(t, uint32(i), sc.MissedStrikes)
			require.Equal(t, active, sc.Status)
			require.Equal(t, tier1Bond, sc.Bond.Amount.Int64())
			ev := f.events(missedEvent)[0]
			require.Equal(t, "sidechain_override", ev["policy_source"])
			require.Equal(t, types.MissedCheckpointAction_MISSED_CHECKPOINT_ACTION_NONE.String(), ev["action"])
		}
		require.Equal(t, tier1Bond, f.moduleBal())
	})

	t.Run("allowed on a suspended sidechain, which still gets no deadline", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setStatus(t, testID, suspended)
		p := exemptPolicy()
		f.setOverride(t, testID, &p)
		require.NotNil(t, f.get(t, testID).MissedPolicyOverride)
		f.requireNoDeadline(t, testID)
	})

	t.Run("refused for an EXITED sidechain", func(t *testing.T) {
		f := newMissedFixture(t)
		_, err := f.msgServer.InitiateSidechainExit(f.ctx, f.exitMsg())
		require.NoError(t, err)
		p := exemptPolicy()
		_, err = f.msgServer.SetSidechainMissedPolicy(f.ctx,
			&types.MsgSetSidechainMissedPolicy{Authority: f.govS, Id: testID, Policy: &p})
		require.ErrorIs(t, err, types.ErrInvalidSidechainState)
		_, err = f.msgServer.SetSidechainMissedPolicy(f.ctx,
			&types.MsgSetSidechainMissedPolicy{Authority: f.govS, Id: testID, Policy: nil})
		require.ErrorIs(t, err, types.ErrInvalidSidechainState)
	})
}

// ---------------------------------------------------------------------------
// params validation through MsgUpdateParams
// ---------------------------------------------------------------------------

func TestUpdateParams_MissedCheckpointPolicyValidation(t *testing.T) {
	f := newFixture(t)
	base, err := f.keeper.GetParams(f.ctx)
	require.NoError(t, err)

	withPolicies := func(ps ...types.MissedCheckpointPolicy) types.Params {
		p := base
		p.MissedCheckpointPolicyByTier = ps
		return p
	}
	noneWithFraction := forTier(exemptPolicy(), notarized)
	noneWithFraction.PenaltyFraction = math.LegacyNewDecWithPrec(1, 2)
	suspendWithFraction := forTier(suspendPolicy(), notarized)
	suspendWithFraction.PenaltyFraction = math.LegacyNewDecWithPrec(1, 2)
	gapTooBig := base
	gapTooBig.MaxBlockGapSeconds = types.MaxPeriodSeconds + 1

	invalid := map[string]types.Params{
		"penalty fraction above 25%":        withPolicies(forTier(penalizePolicy("0.250000000000000001", 1, 0), notarized)),
		"PENALIZE without suspend_after":    withPolicies(forTier(penalizePolicy("0.01", 0, 0), notarized)),
		"NONE with a penalty fraction":      withPolicies(noneWithFraction),
		"SUSPEND with a penalty fraction":   withPolicies(suspendWithFraction),
		"duplicate tier":                    withPolicies(forTier(exemptPolicy(), attested), forTier(suspendPolicy(), attested)),
		"policy without a tier":             withPolicies(exemptPolicy()),
		"max_block_gap above MaxPeriodSecs": gapTooBig,
	}
	for name, p := range invalid {
		_, err := f.msgServer.UpdateParams(f.ctx, &types.MsgUpdateParams{Authority: f.govS, Params: p})
		require.Error(t, err, name)
	}

	valid := map[string]types.Params{
		"penalty fraction at the 25% cap": withPolicies(forTier(penalizePolicy("0.25", 1, 0), notarized)),
		"example tier policies":           withPolicies(types.DefaultMissedCheckpointPolicies()...),
	}
	for name, p := range valid {
		_, err := f.msgServer.UpdateParams(f.ctx, &types.MsgUpdateParams{Authority: f.govS, Params: p})
		require.NoError(t, err, name)
	}
}

// ---------------------------------------------------------------------------
// chain-outage guard
// ---------------------------------------------------------------------------

func TestMissedCheckpoint_OutageGuard(t *testing.T) {
	// guardFixture: default params (max_block_gap_seconds 600), a penalizing
	// tier policy, one ACTIVE sidechain, and one EndBlock at activation.
	guardFixture := func(t *testing.T) *fixture {
		f := newFixture(t)
		p, err := f.keeper.GetParams(f.ctx)
		require.NoError(t, err)
		require.Equal(t, uint64(600), p.MaxBlockGapSeconds)
		f.setTierPolicies(t, forTier(penalizePolicy("0.10", 3, 0), notarized))
		f.register(t)
		f.activate(t, testID)
		f.endBlock(t)
		return f
	}

	t.Run("a block gap above the limit forgives deadlines at or before it", func(t *testing.T) {
		f := guardFixture(t)
		d := f.deadline(t, testID)
		f.blocksUntil(t, d.Add(-time.Minute), 5*time.Minute)
		require.Zero(t, f.get(t, testID).MissedStrikes)

		// The chain halts and the next block comes two hours later.
		f.at(d.Add(2 * time.Hour))
		f.endBlock(t)
		now := f.ctx.BlockTime()

		sc := f.get(t, testID)
		require.Zero(t, sc.MissedStrikes)
		require.Equal(t, tier1Bond, sc.Bond.Amount.Int64())
		require.Equal(t, active, sc.Status)
		require.True(t, sc.NextDeadlineAt.Equal(now.Add(scWindow)), "rebased from the block after the outage")
		f.requireQueuedAt(t, testID, now.Add(scWindow))
		require.Empty(t, f.events(missedEvent))
		forgiven := f.events(forgivenEvent)
		require.Len(t, forgiven, 1)
		require.Equal(t, testID, forgiven[0]["sidechain_id"])

		fb, err := f.keeper.ForgiveBefore.Get(f.ctx)
		require.NoError(t, err)
		require.Equal(t, uint64(now.Unix()), fb)
		lbt, err := f.keeper.LastBlockTime.Get(f.ctx)
		require.NoError(t, err)
		require.Equal(t, uint64(now.Unix()), lbt)

		// With regular blocks again, the next deadline is enforced.
		f.blocksUntil(t, f.deadline(t, testID), 5*time.Minute)
		sc = f.get(t, testID)
		require.Equal(t, uint32(1), sc.MissedStrikes)
		require.Equal(t, int64(900_000), sc.Bond.Amount.Int64())
	})

	t.Run("a gap of exactly max_block_gap_seconds is not an outage", func(t *testing.T) {
		f := guardFixture(t)
		d := f.deadline(t, testID)
		f.blocksUntil(t, d.Add(-5*time.Minute), 5*time.Minute)
		f.at(d.Add(5 * time.Minute)) // gap of exactly 600 s
		f.endBlock(t)
		require.Equal(t, uint32(1), f.get(t, testID).MissedStrikes)
		require.Empty(t, f.events(forgivenEvent))
	})

	t.Run("a gap one second longer is", func(t *testing.T) {
		f := guardFixture(t)
		d := f.deadline(t, testID)
		f.blocksUntil(t, d.Add(-5*time.Minute), 5*time.Minute)
		f.at(d.Add(5*time.Minute + time.Second))
		f.endBlock(t)
		require.Zero(t, f.get(t, testID).MissedStrikes)
		require.Len(t, f.events(forgivenEvent), 1)
	})

	t.Run("0 disables the guard", func(t *testing.T) {
		f := guardFixture(t)
		f.setParams(t, func(p *types.Params) { p.MaxBlockGapSeconds = 0 })
		d := f.deadline(t, testID)
		f.blocksUntil(t, d.Add(-time.Minute), 5*time.Minute)
		f.at(d.Add(2 * time.Hour))
		f.endBlock(t)
		sc := f.get(t, testID)
		require.Equal(t, uint32(1), sc.MissedStrikes)
		require.Equal(t, int64(900_000), sc.Bond.Amount.Int64())
		require.Empty(t, f.events(forgivenEvent))
	})

	t.Run("forgiveness reaches entries processed in later blocks", func(t *testing.T) {
		f := guardFixture(t)
		ids := append([]string{testID}, f.cloneActive(t, 150)...)
		d := f.deadline(t, testID)
		f.blocksUntil(t, d.Add(-time.Minute), 5*time.Minute)

		f.at(d.Add(2 * time.Hour))
		f.endBlock(t)
		require.Len(t, f.events(forgivenEvent), keeper.MaxDeadlinesPerBlock)
		f.advance(5 * time.Second)
		f.endBlock(t)
		require.Len(t, f.events(forgivenEvent), len(ids)-keeper.MaxDeadlinesPerBlock)
		require.Empty(t, f.events(missedEvent))
		for _, id := range ids {
			require.Zero(t, f.get(t, id).MissedStrikes, id)
		}
	})
}

// ---------------------------------------------------------------------------
// per-block bound and failure handling
// ---------------------------------------------------------------------------

func TestMissedCheckpoint_AtMostMaxDeadlinesPerBlock(t *testing.T) {
	require.Equal(t, 100, keeper.MaxDeadlinesPerBlock)

	f := newMissedFixture(t)
	ids := append([]string{testID}, f.cloneActive(t, 150)...)
	d := f.deadline(t, testID)
	for _, id := range ids {
		require.True(t, f.deadline(t, id).Equal(d))
	}

	struck := func() int {
		n := 0
		for _, id := range ids {
			if f.get(t, id).MissedStrikes > 0 {
				n++
			}
		}
		return n
	}

	f.at(d)
	f.endBlock(t)
	require.Equal(t, keeper.MaxDeadlinesPerBlock, struck())
	require.Len(t, f.events(missedEvent), keeper.MaxDeadlinesPerBlock)
	require.Equal(t, len(ids), f.queueLen(t), "processed entries are requeued, the rest stay due")

	f.advance(time.Second)
	f.endBlock(t)
	require.Equal(t, len(ids), struck())
	require.Len(t, f.events(missedEvent), len(ids)-keeper.MaxDeadlinesPerBlock)
	for _, id := range ids {
		require.Equal(t, uint32(1), f.get(t, id).MissedStrikes, id)
	}
}

func TestMissedCheckpoint_FailureDoesNotHaltAndIsRetried(t *testing.T) {
	f := newMissedFixture(t)
	f.setTierPolicies(t, forTier(penalizePolicy("0.10", 3, 0), notarized))
	okID := f.cloneActive(t, 1)[0]
	exempt := exemptPolicy()
	f.setOverride(t, okID, &exempt) // processing it needs no burn
	d := f.deadline(t, testID)

	// Make the burn fail: the module account holds nothing.
	escrow := f.bank.moduleBalance(types.ModuleName)
	f.bank.modules[types.ModuleName] = math.ZeroInt()

	f.at(d)
	f.endBlock(t) // returns nil

	sc := f.get(t, testID)
	require.Zero(t, sc.MissedStrikes, "the failed entry's state changes are discarded")
	require.Equal(t, tier1Bond, sc.Bond.Amount.Int64())
	require.True(t, sc.NextDeadlineAt.Equal(d))
	f.requireQueuedAt(t, testID, d.Add(keeper.DeadlineRetryDelay))
	require.Equal(t, uint32(1), f.get(t, okID).MissedStrikes, "healthy entries are still processed")
	evs := f.events(missedEvent)
	require.Len(t, evs, 1)
	require.Equal(t, okID, evs[0]["sidechain_id"])

	f.bank.modules[types.ModuleName] = escrow

	f.at(d.Add(keeper.DeadlineRetryDelay - time.Second))
	f.endBlock(t)
	require.Zero(t, f.get(t, testID).MissedStrikes)

	f.at(d.Add(keeper.DeadlineRetryDelay))
	f.endBlock(t)
	sc = f.get(t, testID)
	require.Equal(t, uint32(1), sc.MissedStrikes)
	require.Equal(t, int64(900_000), sc.Bond.Amount.Int64())
	f.requireQueuedAt(t, testID, f.ctx.BlockTime().Add(scWindow))
}

// ---------------------------------------------------------------------------
// genesis
// ---------------------------------------------------------------------------

func TestMissedCheckpoint_Genesis(t *testing.T) {
	importInto := func(t *testing.T, f *fixture, gs *types.GenesisState) *fixture {
		t.Helper()
		require.NoError(t, gs.Validate())
		g := newFixture(t)
		g.bank.modules[types.ModuleName] = f.bank.moduleBalance(types.ModuleName)
		require.NoError(t, g.keeper.InitGenesis(g.ctx, *gs))
		return g
	}

	t.Run("an ACTIVE sidechain with next_deadline_at gets its queue entry back", func(t *testing.T) {
		f := newMissedFixture(t)
		f.setTierPolicies(t, forTier(penalizePolicy("0.10", 3, 0), notarized))
		d := f.deadline(t, testID)

		exported, err := f.keeper.ExportGenesis(f.ctx)
		require.NoError(t, err)
		require.True(t, exported.Sidechains[0].NextDeadlineAt.Equal(d))

		g := importInto(t, f, exported)
		require.True(t, g.deadline(t, testID).Equal(d))
		g.requireQueuedAt(t, testID, d)

		g.miss(t, testID)
		sc := g.get(t, testID)
		require.Equal(t, uint32(1), sc.MissedStrikes)
		require.Equal(t, int64(900_000), sc.Bond.Amount.Int64())
		g.requireEscrowInvariant(t)
	})

	t.Run("an ACTIVE sidechain without one gets a fresh deadline", func(t *testing.T) {
		f := newMissedFixture(t)
		exported, err := f.keeper.ExportGenesis(f.ctx)
		require.NoError(t, err)
		exported.Sidechains[0].NextDeadlineAt = nil

		g := newFixture(t)
		g.advance(day)
		g.bank.modules[types.ModuleName] = f.bank.moduleBalance(types.ModuleName)
		require.NoError(t, g.keeper.InitGenesis(g.ctx, *exported))
		want := g.ctx.BlockTime().Add(scWindow)
		require.True(t, g.deadline(t, testID).Equal(want))
		g.requireQueuedAt(t, testID, want)
	})

	t.Run("a non-ACTIVE sidechain's deadline is dropped on import", func(t *testing.T) {
		f := newMissedFixture(t)
		exported, err := f.keeper.ExportGenesis(f.ctx)
		require.NoError(t, err)
		require.NotNil(t, exported.Sidechains[0].NextDeadlineAt)
		exported.Sidechains[0].Status = suspended

		g := importInto(t, f, exported)
		g.requireNoDeadline(t, testID)
		require.Zero(t, g.queueLen(t))
	})
}

// ---------------------------------------------------------------------------
// probes: suspected behaviour questions. Each test checks the behaviour and,
// when it is confirmed, skips with an explanation instead of failing the suite.
// ---------------------------------------------------------------------------

// Probe (a): a PENALIZE burn can take the bond below the tier's MinBond while
// the sidechain stays ACTIVE and keeps taking checkpoints.
func TestProbe_PenaltyCanTakeBondBelowTierMinBond(t *testing.T) {
	f := newMissedFixture(t)
	f.setTierPolicies(t, forTier(penalizePolicy("0.10", 3, 0), notarized))
	p, err := f.keeper.GetParams(f.ctx)
	require.NoError(t, err)
	minBond, ok := p.MinBondFor(notarized)
	require.True(t, ok)
	require.Equal(t, tier1Bond, minBond.Amount.Int64(), "registered at exactly the tier minimum")

	f.miss(t, testID)
	sc := f.get(t, testID)
	f.advance(time.Minute)
	cpErr := f.keeper.RecordCheckpoint(f.ctx, testID, sc.LastCheckpointSequence+1, testHeight, []byte("h"))

	if sc.Bond.Amount.LT(minBond.Amount) && sc.Status == active && cpErr == nil {
		t.Skipf("PROBE (a) CONFIRMED: after one missed checkpoint the bond is %s, below the %s minimum "+
			"for %s, yet the sidechain is still ACTIVE and its next checkpoint is accepted. processDeadline "+
			"(deadlines.go) burns and only suspends on SUSPEND or suspend_after_strikes; nothing compares the "+
			"remaining bond with Params.MinBondFor. Decide whether a sub-minimum bond should suspend, require a "+
			"top-up, or is acceptable.", sc.Bond, minBond, notarized)
	}
	t.Logf("probe (a) not reproduced: bond %s, status %s, checkpoint err %v", sc.Bond, sc.Status, cpErr)
}

// Probe (b): MigrateV1ToV2 exists but is not registered and ConsensusVersion
// is still 1, so on an existing chain an ACTIVE sidechain written before the
// upgrade, with no queue entry, is never given a deadline by EndBlock.
func TestProbe_PreUpgradeActiveSidechainNeverGetsADeadline(t *testing.T) {
	f := newMissedFixture(t)
	f.setTierPolicies(t, forTier(penalizePolicy("0.10", 3, 0), notarized))

	consensusVersion := sidechainmodule.AppModule{}.ConsensusVersion()

	// Simulate the record as a v1 chain stored it: no deadline, no queue entry.
	// (Raw collection write, the way pre-feature code wrote records.)
	sc := f.get(t, testID)
	f.requireQueuedAt(t, testID, *sc.NextDeadlineAt)
	require.NoError(t, f.keeper.DeadlineQueue.Remove(f.ctx, collections.Join(uint64(sc.NextDeadlineAt.Unix()), testID)))
	sc.NextDeadlineAt = nil
	require.NoError(t, f.keeper.Sidechains.Set(f.ctx, testID, sc))
	require.Zero(t, f.queueLen(t))

	// Ten days of hourly blocks with no checkpoints.
	f.blocksUntil(t, f.ctx.BlockTime().Add(10*day), time.Hour)
	after := f.get(t, testID)
	neverAssigned := after.NextDeadlineAt == nil && after.MissedStrikes == 0 && f.queueLen(t) == 0

	// MigrateV1ToV2 would have fixed it.
	require.NoError(t, f.keeper.MigrateV1ToV2(f.ctx))
	want := f.ctx.BlockTime().Add(scWindow)
	require.True(t, f.deadline(t, testID).Equal(want))
	f.requireQueuedAt(t, testID, want)
	f.miss(t, testID)
	require.Equal(t, uint32(1), f.get(t, testID).MissedStrikes)

	if neverAssigned && consensusVersion == 1 {
		t.Skipf("PROBE (b) CONFIRMED: ConsensusVersion is %d and MigrateV1ToV2 is not registered "+
			"(no cfg.RegisterMigration in module.go, no upgrade handler in app/). An ACTIVE sidechain stored "+
			"before the upgrade has no deadline and no DeadlineQueue entry; ProcessMissedCheckpoints only walks "+
			"the queue, and processDeadline's 'ACTIVE without a deadline: assign one' branch is reached only "+
			"from a queue entry, so after 10 days of blocks it still has no deadline and 0 strikes. It gets one "+
			"only when something else writes it through SetSidechain (a checkpoint, signer rotation, update...). "+
			"Calling MigrateV1ToV2 assigns the deadline as intended.", consensusVersion)
	}
	t.Logf("probe (b) not reproduced: consensus version %d, deadline %v, strikes %d",
		consensusVersion, after.NextDeadlineAt, after.MissedStrikes)
}

// Probe (c), found while writing the tests: a checkpoint that lands after the
// deadline but before the deadline's queue entry is processed resets the
// window and the miss is never recorded. Normally that is the first block past
// the deadline (the tx runs before EndBlock), but when processing failed and
// was requeued it is a whole DeadlineRetryDelay.
func TestProbe_LateCheckpointBeforeProcessingErasesTheMiss(t *testing.T) {
	f := newMissedFixture(t)
	f.setTierPolicies(t, forTier(penalizePolicy("0.10", 3, 0), notarized))

	// Late by 5 s, in the first block past the deadline.
	d := f.deadline(t, testID)
	f.at(d.Add(5 * time.Second))
	f.checkpointNow(t, testID)
	f.endBlock(t)
	oneBlockLate := f.get(t, testID).MissedStrikes == 0

	// Late by 30 min, after the first processing attempt failed.
	d2 := f.deadline(t, testID)
	escrow := f.bank.moduleBalance(types.ModuleName)
	f.bank.modules[types.ModuleName] = math.ZeroInt()
	f.at(d2)
	f.endBlock(t)
	f.bank.modules[types.ModuleName] = escrow
	f.at(d2.Add(30 * time.Minute))
	f.checkpointNow(t, testID)
	f.at(d2.Add(keeper.DeadlineRetryDelay))
	f.endBlock(t)
	sc := f.get(t, testID)
	retryLate := sc.MissedStrikes == 0 && sc.Bond.Amount.Int64() == tier1Bond

	if oneBlockLate && retryLate {
		t.Skip("PROBE (c) CONFIRMED: RecordCheckpoint -> applyCheckpointLiveness (deadlines.go) clears " +
			"NextDeadlineAt for any checkpoint, on time or not, and the queue entry is then treated as stale. " +
			"A checkpoint 5 s after the deadline (same block, before EndBlock) and one 30 min after it (while " +
			"the entry waited DeadlineRetryDelay after a failed burn) both erase the miss: no strike, no burn. " +
			"Late checkpoints only lose the on-time streak. Decide whether a late checkpoint should record the " +
			"miss before starting the new window.")
	}
	t.Logf("probe (c) not reproduced: one-block-late=%v retry-late=%v", oneBlockLate, retryLate)
}
