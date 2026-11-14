package rolebots

// Summit lines: recruitment and shops (summit_trade.go). An ad is bumped
// every few days, so its pools are wide: the least-recently-used pick keeps
// one corporation's ads from reading the same twice in a month.

func init() {
	addSummit("recruit", summitPartTitle, voiceAny,
		"[{usTicker}] {us} is recruiting",
		"{us} is looking for pilots",
		"Join {us}",
		"{us} - recruitment open",
	)
	addSummit("recruit.ad", summitPartTitle, voiceAny,
		"[{usTicker}] {us} is recruiting",
		"{us} wants pilots",
		"Recruiting: {us}",
	)
	addSummit("recruit.ad", summitPartTitle, voiceShowman, "{us}: the best seat in the house is open")
	addSummit("recruit.ad", summitPartTitle, voicePirate, "{us} is hiring. No questions asked.")
	addSummit("recruit.ad", summitPartOpen, voiceAny,
		"{us} is taking applications.",
		"Still recruiting.",
		"Bump. {us} has room for more pilots.",
		"Looking for a corporation that actually flies together? {us} is recruiting.",
		"Back to the top. {us} is still hiring.",
		"New week, same offer: {us} wants pilots.",
	)
	addSummit("recruit.ad", summitPartOpen, voiceCorporate, "{us} is expanding and has openings for disciplined capsuleers.")
	addSummit("recruit.ad", summitPartOpen, voiceTribal, "{us} is a clan, not a payroll. If that sounds right to you, come and talk.")
	addSummit("recruit.ad", summitPartOpen, voiceZealot, "{us} seeks pilots of conviction.")
	addSummit("recruit.ad", summitPartOpen, voiceTerse, "{us}. Recruiting.")
	addSummit("recruit.ad", summitPartFact, voiceAny,
		"We are {members} pilots and growing.",
		"Our headquarters is at {hq}.",
		"We do {work}, and we do it properly.",
		"Mostly {work}, with a fleet when one is needed.",
		"Our pilots killed {kills} ships this past week.",
		"{kills} kills in the last seven days. We are not a quiet outfit.",
		"New pilots welcome. We will teach you what you need.",
		"Veterans welcome too, if you can follow an order.",
		"Corporate hangars, a buyback and fleets most days.",
		"Send an application through the corporation window and someone will read it.",
		"Based out of {hq}. You will need to move there.",
	)
	addSummit("recruit.ad", summitPartClose, voiceAny,
		"Applications open.",
		"Apply in game.",
		"Questions in this thread.",
		"See you in corp chat.",
	)

	addSummit("shop", summitPartTitle, voiceAny,
		"Selling: {item} and more",
		"WTS {item}",
		"Small shop, fair prices",
	)
	addSummit("shop.list", summitPartTitle, voiceAny,
		"WTS {item}",
		"Selling {item} at {station}",
		"For sale: {item} and other things",
		"{item} on the market at {station}",
	)
	addSummit("shop.list", summitPartOpen, voiceAny,
		"New stock up.",
		"Fresh listing.",
		"Restocked.",
		"Bumping with something new.",
		"Put some more on the market today.",
	)
	addSummit("shop.list", summitPartOpen, voicePirate, "Fresh off the line. Do not ask which line.")
	addSummit("shop.list", summitPartOpen, voiceCorporate, "New inventory is available.")
	addSummit("shop.list", summitPartFact, voiceAny,
		"{units} {item} at {price} each, {station}.",
		"{item}, {units} of them, {price} apiece.",
		"Listed {units} {item} for {total} in total.",
		"You will find them at {station} in {system}.",
		"Market order, so just buy it. No haggling, no contracts.",
		"Priced to move.",
	)
	addSummit("shop.list", summitPartClose, voiceAny, "First come, first served.", "Fly safe.", "More next week.")
}
