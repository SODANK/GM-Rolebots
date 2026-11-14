package rolebots

import "context"

// The planner's ladder: one row per pre-emption, in the order plan() (sim.go) tries them.
// Each row's fn is a method expression; the gates (managed corporation only, a conscript
// only, starbase power on) live inside the fns.
//
//   - handled wins. A row that reports handled and an error has set the pilot's phase
//     (researchCollectDue, industryCollectDue), so the error is logged and the ladder stops.
//   - an effect-only row never pre-empts. repairUpkeep and implantUpkeep act where the
//     pilot stands and planning goes on.
//
// TestPlanLadderOrder pins the sequence: add a row where it belongs and update its list.

type preempt struct {
	name       string                                             // short, for the golden test and logs
	cat        string                                             // logBot category
	msg        string                                             // what the error log says this was doing
	fn         func(*Mod, context.Context, *botRun) (bool, error) // (handled, err)
	effectOnly bool                                               // runs for its side effect, never pre-empts
}

var planLadder = []preempt{
	// A pilot in a capsule buys a ride out of it (shuttle.go). It is about the pilot, not the
	// next fit; procure.dropship then leaves the shuttle at home.
	{name: "ride-home", cat: "procure", msg: "looking for a ride home", fn: (*Mod).rideHome},

	// A pilot still living in its character-creation station moves out, once (moveout.go).
	// It sits ahead of the clone check so the next clone goes in the new home.
	{name: "move-out", cat: "clone", msg: "looking for somewhere of its own", fn: (*Mod).moveOut},

	// A pilot that changed employer moves its clone and factory to it (corphome.go). The
	// address must settle before the clone check buys a clone that pins it elsewhere.
	{name: "corp-home", cat: "corp", msg: "moving in with its employer", fn: (*Mod).corpHomeMove},

	// A pilot whose clone no longer covers its skill points fixes that first (clone.go).
	// Reporting travelling means it is already on its way to a medical bay.
	{name: "clone", cat: "clone", msg: "upkeep", fn: (*Mod).cloneUpkeep},

	// Docked, a pilot buys wanted implants listed at this dock within its budget
	// (implantupkeep.go). Effect-only: nobody travels for one.
	{name: "implants", cat: "implants", msg: "upkeep", fn: (*Mod).implantUpkeep, effectOnly: true},

	// A damaged hull is mended through the player repair-shop transaction (repair.go).
	// Effect-only: repair never sends the pilot anywhere.
	{name: "repair", cat: "repair", msg: "upkeep", fn: (*Mod).repairUpkeep, effectOnly: true},

	// A finished manufacturing job is collected before any other errand (industry.go).
	// Skipping it strands the blueprint at flag 111 and the demand claim goes stale.
	{name: "industry-collect", cat: "industry", msg: "collecting a finished job", fn: (*Mod).industryCollectDue},

	// A won Tech II patent is collected on the same terms (research.go): nothing else
	// fetches it before the claim deadline. No pending patent costs a stamp comparison.
	{name: "research-collect", cat: "research", msg: "collecting a won patent", fn: (*Mod).researchCollectDue},

	// A Mind Clash booking sends the pilot to the venue (clash_travel.go); a stub when the
	// mindclash tag is off (clash_off.go). A map read on the common pass.
	{name: "clash", cat: "clash", msg: "venue errand", fn: (*Mod).clashVenueDue},

	// A finished research job is collected from flag 111 (labwork.go); ownedCandidates only
	// sees flag 4. A pilot with no lab errand costs two integer comparisons.
	{name: "lab-collect", cat: "lab", msg: "collecting a researched blueprint", fn: (*Mod).labCollectDue},

	// A purchase order is corporation money in this pilot's wallet to buy a print for the
	// library (corplibrary.go). An unfilled order is voided and the advance clawed back.
	{name: "corp-po", cat: "industry", msg: "corporation purchase order", fn: (*Mod).corpPODue},

	// A factory move carries corporation stock the desk can no longer reach
	// (corpfactorymove.go). Only the director and deputy carry it; others do a map read.
	{name: "corp-move", cat: "industry", msg: "corporation factory move", fn: (*Mod).corpMoveDue},

	// A print moves between two corporation bases (corpprintmove.go): an original by a
	// Director or the CEO, a copy by a member posted where it sits.
	{name: "print-move", cat: "industry", msg: "carrying a blueprint between bases", fn: (*Mod).printMoveDue},

	// The starbase boards (postask.go): a haul leg, a buy trip, a survey. Fuel and kit run
	// on a deadline. A map read for anyone whose corporation has posted none.
	{name: "pos-errand", cat: "corp", msg: "starbase errand", fn: (*Mod).posErrandDue},

	// A letter in hand (corpenvoy.go): the corporation's proposal expires in three days and
	// the envoy seat carries it. A map read for everyone else.
	{name: "envoy", cat: "corp", msg: "carrying a letter", fn: (*Mod).envoyErrandDue},

	// A player's fleet outranks the gang gate, so this sits above it but below the
	// collections. A conscript with no standing order still lands here: its default duty is
	// to stay with the fleet, not to fall through to planSolo.
	{name: "conscript", cat: "conscript", msg: "planning", fn: (*Mod).conscriptPlan},

	// A loot ledger nobody sells (loothaul.go): a full hold, or a haul older than six hours,
	// is sold at this dock. It sits below conscript and above the gang gate and planSolo.
	{name: "loot-haul", cat: "market", msg: "disposing of a loot haul", fn: (*Mod).lootHaulDue},
}

// runLadder tries every pre-emption in order. True means one of them took
// the pilot and planning stops here.
func (m *Mod) runLadder(ctx context.Context, b *botRun) bool {
	for _, p := range planLadder {
		handled, err := p.fn(m, ctx, b)
		if err != nil {
			m.logBot(b.CharID, logError, p.cat, p.msg+": %v", err)
		}
		if handled && !p.effectOnly {
			return true
		}
	}
	return false
}

// planLadderNames is the sequence, for the golden test.
func planLadderNames() []string {
	out := make([]string, 0, len(planLadder))
	for _, p := range planLadder {
		out = append(out, p.name)
	}
	return out
}
