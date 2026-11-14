package rolebots

// The camp's lines (camp.go): the cargo demand and the checkpoint's three.
// {target} is the mark's name as a link, {isk} the figure.

const (
	evCargoDemand chatEvent = "cargodemand" // a pirate asking for the hold itself
	evTollDemand  chatEvent = "tolldemand"  // a checkpoint's price
	evTollPaid    chatEvent = "tollpaid"    // the toll landed
	evWaveThrough chatEvent = "wave"        // a checkpoint letting a pilot pass
)

func init() {
	addLines(evCargoDemand, motivePiracy, voiceAny,
		"{target}. Not your money — your hold. Jettison it and you fly home.",
		"{target}, I've seen what you're carrying. {isk} of it. Drop it, all of it, now.",
		"Cargo, {target}. Every can. Then the point comes off.",
		"{target}: the scan says {isk}. That's the price, and you pay it in cargo.",
		"Open the hold and jettison, {target}. I'll take it from there.",
	)
	addLines(evCargoDemand, motivePiracy, voicePirate,
		"{target}, your hold's worth more than your word. Drop it.",
		"Jettison, {target}. The ship's yours, the {isk} inside it isn't.",
	)
	addLines(evCargoDemand, motivePiracy, voiceCold,
		"{target}. Jettison. {isk}.",
	)

	addLines(evTollDemand, motivePiracy, voiceAny,
		"{target}, this gate is under contract. {isk} and you're through.",
		"Toll, {target}. {isk}. Nothing personal — the client pays us to stand here.",
		"{target}: {isk} to pass. Pay it and we never met.",
		"Checkpoint, {target}. {isk}, one minute, then the point comes off either way.",
	)
	addLines(evTollDemand, motivePiracy, voiceCorporate,
		"{target}, transit fee: {isk}. Invoice is in Local.",
	)
	addLines(evTollDemand, motivePiracy, voiceCold,
		"{target}. {isk}. Then go.",
	)

	addLines(evTollPaid, motiveNone, voiceAny,
		"Received. {target} has the hour.",
		"Paid. Fly on, {target}.",
		"{target} is good for the hour. Point's off.",
		"Thank you for your custom, {target}.",
	)

	addLines(evWaveThrough, motiveNone, voiceAny,
		"{target}, you're clear. Move along.",
		"Checkpoint. {target}, nothing to see here — carry on.",
		"{target}, through you go.",
		"Clear, {target}. Safe travels.",
		"{target}: not on the list. Pass.",
	)
	addLines(evWaveThrough, motiveNone, voiceCorporate,
		"{target}, papers in order. Proceed.",
	)
	addLines(evWaveThrough, motiveNone, voiceZealot,
		"{target}, go in grace. This gate is watched.",
	)
}
