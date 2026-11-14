package rolebots

import (
	"context"
	"fmt"
	"time"
)

// The starbase errands' tasks and the planner's one pre-emption for all of them. None is
// Interruptible: a carrier holds corporation cargo, a buyer the corporation's advance, a surveyor
// its probes, and a muster call that pulled any of them away would strand the corporation's property.

const (
	// TaskPosHaul carries a haul-board leg (poshaul.go).
	TaskPosHaul TaskKind = "starbase.haul"
	// TaskPosBuy is the Procurer's buy trip (posreq.go).
	TaskPosBuy TaskKind = "starbase.buy"
	// TaskPosSurvey is a moon survey (posintel.go).
	TaskPosSurvey TaskKind = "starbase.survey"
)

func init() {
	registerTask(TaskDef{
		Kind: TaskPosHaul, Label: "Hauling for its corporation's starbases",
		Posture: PostureHold, Detail: detailPosHaul,
		Step: (*Mod).posHaulStep,
	})
	registerTask(TaskDef{
		Kind: TaskPosBuy, Label: "Buying its corporation's supplies",
		Posture: PostureDocked, Detail: detailPosBuy,
		Step: (*Mod).posBuyStep,
	})
	registerTask(TaskDef{
		Kind: TaskPosSurvey, Label: "Surveying moons for its corporation",
		Posture: PostureHold, Detail: detailPosSurvey,
		Step: (*Mod).posSurveyStep,
	})
}

func detailPosHaul(m *Mod, ctx context.Context, st *SimState) string {
	l, err := m.loadHaul(ctx, st.Data.PosLegID)
	if err != nil || l == nil {
		return ""
	}
	verb := "loading"
	if l.carryingAny() {
		verb = "carrying"
	}
	return fmt.Sprintf("%s a %s leg (%.0f m³) to %s", verb, l.Kind, l.M3, m.haulDestLabel(ctx, l))
}

func detailPosBuy(m *Mod, ctx context.Context, st *SimState) string {
	if st.Data.PosTripStation != 0 {
		return fmt.Sprintf("%d requisition line(s) to buy at %s", len(st.Data.PosTripReqs), m.stationLabel(ctx, st.Data.PosTripStation))
	}
	return fmt.Sprintf("delivering %d requisition line(s)", len(st.Data.PosTripReqs))
}

func detailPosSurvey(m *Mod, ctx context.Context, st *SimState) string {
	so, err := m.loadSurvey(ctx, st.Data.PosSurveyID)
	if err != nil || so == nil {
		return ""
	}
	return fmt.Sprintf("%s: %d moon(s) surveyed", systemNameOf(ctx, m, so.SystemID), so.MoonsDone)
}

// posErrandDue is plan()'s pre-emption for the corporation's boards: a pilot of a bot-run
// corporation with errands posted and nothing in hand carries a leg, makes a buy trip or runs a
// survey before its own appetites get a say; for everybody else it is a map read. Only the
// tower-specific errands (staging, tower jobs, surveys) are behind StarbasePower.
func (m *Mod) posErrandDue(ctx context.Context, b *botRun) (bool, error) {
	if b == nil || b.CorpID == 0 || b.State == nil {
		return false, nil
	}
	if _, ok := managedCEO(b.CorpID); !ok {
		return false, nil
	}
	// A leg in hand is finished whatever the marks say.
	if b.State.Data.IndTypeID != 0 {
		m.noteBuyerBusy(ctx, b, "it is in the middle of an industry job")
		return false, nil
	}
	towers := StarbasePower()
	if !corpHasPosWork(b.CorpID) {
		// Residents stage at their tower now and then whatever the boards
		// say (posresident.go).
		if towers {
			if handled, err := m.towerStageDue(ctx, b); err != nil || handled {
				return handled, err
			}
		}
		// Still resume a leg this pilot was carrying, or buy the lines it was handed, when the marks
		// were cleared (a restart empties them). Who is carrying or buying is shared for a minute.
		busy, _ := snapshotFor(snapKey("pos-carriers"), time.Minute, func() (map[int64]bool, error) {
			return m.corpIDsWhere(ctx, `
				SELECT carrierID FROM rolebot_corp_haul WHERE status = 'claimed'
				UNION
				SELECT buyerID FROM rolebot_corp_requisition WHERE status = 'buying'`)
		})
		if !busy[b.CharID] {
			return false, nil
		}
	}
	if handled, err := m.posHaulDue(ctx, b); err != nil || handled {
		return handled, err
	}
	if handled, err := m.posBuyDue(ctx, b); err != nil || handled {
		return handled, err
	}
	if !towers {
		return false, nil
	}
	if handled, err := m.posTowerJobDue(ctx, b); err != nil || handled {
		return handled, err
	}
	if handled, err := m.posTowerRefineDue(ctx, b); err != nil || handled {
		return handled, err
	}
	if handled, err := m.posSurveyDue(ctx, b); err != nil || handled {
		return handled, err
	}
	return m.towerStageDue(ctx, b)
}

// noteBuyerBusy logs, through reqDecline's throttle, that a buyer holding
// 'buying' lines is passing them by this plan and why. One indexed count for
// a pilot that holds none.
func (m *Mod) noteBuyerBusy(ctx context.Context, b *botRun, why string) {
	reqs, err := m.queryReqs(ctx, `WHERE buyerID = ? AND status = 'buying' ORDER BY deadline, reqID LIMIT 1`, b.CharID)
	if err != nil || len(reqs) == 0 {
		return
	}
	m.reqDecline(ctx, b.CharID, &reqs[0], why)
}
