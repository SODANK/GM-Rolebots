package rolebots

import (
	"math"

	"gurimoon/destiny"
)

// A live ball follows its pilot's corporation, alliance and security status. The slim item is built
// once at birth (prepareProjection); the roster-wide reconcile compares it with the roster row each
// pass and tells the witnesses when it moved. See docs/comment-notes-b07.md#slim-refresh.

// slimIdentity is the part of a ball's slim item that belongs to the pilot and not to the hull.
type slimIdentity struct {
	corpID, allianceID int64
	secStatus          float64
}

// slimSecEpsilon is how far two security readings may differ and still be one value: the roster
// reads the column back from the database and the core sets the slim from its own arithmetic.
const slimSecEpsilon = 1e-4

func identityOf(s destiny.SlimItem) slimIdentity {
	return slimIdentity{corpID: s.CorpID, allianceID: s.AllianceID, secStatus: s.SecurityStatus}
}

// same reports whether two identities would read alike on a client.
func (a slimIdentity) same(b slimIdentity) bool {
	return a.corpID == b.corpID && a.allianceID == b.allianceID &&
		math.Abs(a.secStatus-b.secStatus) < slimSecEpsilon
}

// refreshIdentity brings a live ball's slim item in line with the roster row and reports whether
// its witnesses hold something else. It also answers true for a change the core made on its own
// (a security penalty sets the slim and tells nobody when the offender is a projection). The caller
// holds embodyMu; the send is pushSlimChange's, off that lock.
func (eb *embodiedBot) refreshIdentity(w embodyWant) bool {
	if eb.ent == nil {
		return false
	}
	eb.corpID = w.corpID
	want := slimIdentity{corpID: w.corpID, allianceID: w.allianceID, secStatus: w.secStatus}
	if !identityOf(eb.ent.Slim).same(want) {
		// Bare writes, as the core's own (cmd/gurimoon secstatus.go): Slim has no lock.
		eb.ent.Slim.CorpID = want.corpID
		eb.ent.Slim.AllianceID = want.allianceID
		eb.ent.Slim.SecurityStatus = want.secStatus
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	return eb.slimDirty || !eb.slimSent.same(identityOf(eb.ent.Slim))
}

// pushSlimChange sends the ball's current slim item to every witness holding the ball, the
// OnSlimItemChange a player's ship sends. A witness whose AddBalls for this ball is still queued is
// skipped and the change stays owed: that AddBalls carries the slim as it was when queued, and a
// change sent ahead of it is dropped by the client. embodyTick sends it again.
func (m *Mod) pushSlimChange(eb *embodiedBot) {
	if eb == nil || eb.ss == nil || eb.ent == nil {
		return
	}
	eb.mu.Lock()
	dead := eb.dead
	eb.mu.Unlock()
	if dead {
		return
	}
	slim := eb.ent.Slim
	now := identityOf(slim)
	sent, owed := 0, false
	for _, mate := range eb.ss.GetBubbleConnectedPlayersExcept(eb.ent.ID) {
		if mate.Conn == nil || !mate.KnowsBall(eb.ent.ID) {
			continue
		}
		if mate.HasPendingAdd(eb.ent.ID) {
			owed = true
			continue
		}
		// OnSlimItemChange spends no stamp of its own, so it rides the witness's current one.
		if err := mate.SendFXStamped(func(stamp uint32) error {
			return destiny.SendSlimItemChange(mate.Conn, m.nodeID, mate.AccountID, stamp, slim)
		}); err != nil {
			rlogf("rolebots: slim change to %s: %v", mate.Addr, err)
			continue
		}
		sent++
	}
	eb.mu.Lock()
	eb.slimDirty = owed
	if !owed {
		eb.slimSent = now
	}
	eb.mu.Unlock()
	if sent > 0 {
		m.logBot(eb.charID, logInfo, "embody",
			"identity updated on grid — corporation %d, alliance %d, security %.2f, %d witness(es) told",
			now.corpID, now.allianceID, now.secStatus, sent)
	}
}
