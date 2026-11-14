package rolebots

// The gank fleet's lines (gank_op.go, gank_embodied.go).
//
// The CALL is said on the fleet's mirrored channel only (fleetSay): it is an
// order, and the mark must not read it in Local before the strikers land.
// The LOOT line is a pirate line in Local, unbudgeted like the ransom demand
// (piracy_voice.go pirateSay): it is the scout saying, to whoever is left on
// the gate, whose wreck this was.

const (
	evGankCall    chatEvent = "gankcall"    // the scout's call, fleet channel only
	evGankLoot    chatEvent = "gankloot"    // the scout over the wrecks, in Local
	evGankRun     chatEvent = "gankrun"     // a striker running from CONCORD, in Local
	evGankBrawl   chatEvent = "gankbrawl"   // a striker turning on CONCORD, in Local
	evGankEscaped chatEvent = "gankescaped" // a striker that got clear, in Local
)

// gankCallLines are fleetSay formats: the mark's name, the hold's worth, the
// ships asked for.
var gankCallLines = []string{
	"Mark on the gate: %s, %s in the hold. %d of you — jump, jump, jump.",
	"%s just landed carrying %s. Send %d. Go now.",
	"Scan says %s. %s. %d destroyers, on the gate, now.",
	"Target %s. Hold reads %s. %d in, lock and burn.",
}

func init() {
	addLines(evGankLoot, motivePiracy, voiceAny,
		"Thanks for the cargo. Insurance will cover the rest.",
		"Nothing personal. {isk} says it was worth the paperwork.",
		"Wreck's open, wreck's empty. Fly safe.",
		"Police took my friends. I took the loot. Fair trade.",
		"{isk} off a gate camp. Tell your corp to buy stabs.",
	)
	addLines(evGankLoot, motivePiracy, voicePirate,
		"Salvage rights, friend. {isk} and change.",
		"The guns are CONCORD's problem. The hold is mine.",
	)
	addLines(evGankLoot, motivePiracy, voiceCold,
		"Collected. {isk}.",
	)
	addLines(evGankLoot, motivePiracy, voiceBrash,
		"Ha! {isk} and all it cost was some destroyers. Cheers!",
	)
	// The aftermath (gank_undock.go gankAftermath): CONCORD is landing and
	// the striker has decided.
	addLines(evGankRun, motivePiracy, voiceAny,
		"Not today, officers.",
		"Aligned and gone. Catch me if you can.",
		"Job's done. I'm not staying for the paperwork.",
	)
	addLines(evGankRun, motivePiracy, voiceCold,
		"Disengaging.",
	)
	addLines(evGankBrawl, motivePiracy, voiceAny,
		"Come on then, CONCORD. One of you is coming with me.",
		"If I'm going down, I'm taking a badge with me.",
		"Pointed? Fine. Guns on the police.",
	)
	addLines(evGankBrawl, motivePiracy, voiceBrash,
		"Ha! Bring the whole precinct!",
	)
	addLines(evGankEscaped, motivePiracy, voiceAny,
		"Clear. Tell CONCORD to try harder.",
		"Out. That one's on the house, Local.",
	)
}
