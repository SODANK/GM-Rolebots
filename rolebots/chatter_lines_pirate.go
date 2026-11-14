package rolebots

// A pirate's lines (piracy_voice.go pirateSay). The first entries of each
// cell are the original lines, kept word for word; the rest widen the set and
// give a pirate that speaks in another register something of its own.
// {target} is the victim's name as a link, {isk} the demand.

func init() {
	addLines(evRansom, motivePiracy, voiceAny,
		"{target}. Hold still and pay {isk}, or don't and don't.",
		"{target} — {isk} buys your hull back. You have one minute.",
		"Cargo or clone, {target}. {isk} and I let go.",
		"{target}, this is a toll, not a fight. {isk}.",
		"{target}: {isk} in my wallet inside a minute or you walk home.",
		"Pay up, {target}. {isk}. I have all day and you have one minute.",
		"{target}, {isk} and you keep the ship. Clock's running.",
		"Point's on, {target}. {isk} turns it off.",
		"{target}, the going rate is {isk}. One minute.",
		"{isk}, {target}. Give or take your ship.",
	)
	addLines(evRansom, motivePiracy, voicePirate,
		"{target}, you've been boarded, figuratively. {isk}.",
		"Toll's {isk}, {target}. No receipts.",
		"{target}, this is a robbery. {isk}, wire it now.",
	)
	addLines(evRansom, motivePiracy, voiceCold,
		"{target}. {isk}. Sixty seconds.",
	)
	addLines(evRansom, motivePiracy, voiceBrash,
		"{target}! Your lucky day — I take ISK. {isk}, chop chop.",
	)

	addLines(evRansomPaid, motivePiracy, voiceAny,
		"Good. Off you go, {target}.",
		"Pleasure doing business, {target}. Fly safe.",
		"Paid and released. Tell your friends I keep my word, {target}.",
		"{target} pays. Point's off — go on.",
		"Smart choice, {target}. Go.",
		"{target}, you're free. Don't come back soon.",
		"Received. {target}, enjoy the rest of your flight.",
	)

	addLines(evRansomLate, motivePiracy, voiceAny,
		"Time's up, {target}.",
		"Wrong answer, {target}.",
		"Should have paid, {target}.",
		"That's a shame, {target}.",
		"{target}, you had a minute. You wasted it.",
		"Have it your way, {target}.",
	)

	addLines(evExecute, motivePiracy, voiceAny,
		"{target}. Nothing personal.",
		"Sit still, {target}. This is quick.",
		"Wrong system, {target}.",
		"{target} — you should not be out here.",
		"No deals today, {target}.",
		"{target}, I don't take ISK. I take ships.",
		"{target}. Thanks for the cargo.",
	)
	addLines(evExecute, motivePiracy, voicePirate,
		"{target}, the hold's mine and so's the wreck.",
	)

	addLines(evCamp, motivePiracy, voiceAny,
		"Gate's mine today.",
		"Toll booth is open.",
		"Anything that jumps in here belongs to me.",
		"Camp's up. Come on through, I dare you.",
		"This gate has a new owner.",
		"Jumping in? Bring ISK.",
	)
}
