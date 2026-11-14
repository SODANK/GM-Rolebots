package rolebots

// Market talk lines (chatter_market.go). These are people in a market hall, not players in
// a chat box: no "WTS", no "lol", no talk of servers or bots. A price is said the way
// iskWords writes it. Slots: {item} the type as a Show Info link, {units} how many, {isk} the
// unit price in question, {total} the whole lot, {ref} the region's two-week trade average,
// {pct} how far from it, {station}, {system}, {region}.
// A remark's motive cell is the speaker's career (mktCombat, mktMining, mktIndustry,
// mktTrade); an ad's is mktCheap when priced under the average; an answer's is what it
// answers (mktNoteAd, mktNoteTalk, mktNoteShop). The general cells must read right in every
// context, since a quarter of lines are drawn from them.

func init() {
	// ── ads ───────────────────────────────────────────────────────────────
	addLines(evMarketAd, motiveNone, voiceAny,
		"{units} {item} on the market at {station}, {isk} each.",
		"Just listed {units} {item} at {station}. {isk} a unit.",
		"Anyone in {system} short of {item}? {units} up at {isk} each, {station}.",
		"Put {units} {item} on the board at {station} — {isk} apiece.",
		"{item}, {units} of them, {isk} each. {station}.",
	)
	addLines(evMarketAd, mktCheap, voiceAny,
		"{units} {item} at {station}, {isk} each. Under the going rate — I want the hangar space.",
		"Priced to move: {units} {item}, {isk} a unit at {station}. The region pays {ref}.",
		"{item} at {isk} in {station}. That's below {ref}, so don't sit on it.",
	)
	addLines(evMarketAd, motiveNone, voiceCorporate,
		"Listing notice: {units} {item}, {isk} per unit, {station}. Settlement on the order.",
		"{units} units of {item} available at {station}. {isk} each. Terms are the order's.",
	)
	addLines(evMarketAd, motiveNone, voiceShowman,
		"Fresh stock in {system}! {units} {item}, {isk} each, {station}. Come and see.",
	)
	addLines(evMarketAd, motiveNone, voiceZealot,
		"{units} {item} at {station}, {isk} each. An honest price, before God.",
	)
	addLines(evMarketAd, motiveNone, voiceTribal,
		"{units} {item} at {station}. {isk} each. Fair to anyone who flies free.",
	)
	addLines(evMarketAd, motiveNone, voiceTerse,
		"{item}. {units}. {isk}. {station}.",
	)
	addLines(evMarketAd, motiveNone, voiceBrash,
		"Best {item} in {region} — {units} of them, {isk} a unit, {station}. Don't all rush.",
	)
	addLines(evMarketAd, motiveNone, voicePirate,
		"{units} {item} at {station}, {isk} each. Don't ask where they came from.",
	)

	// ── paid too much ─────────────────────────────────────────────────────
	addLines(evMarketPaid, motiveNone, voiceAny,
		"Just paid {isk} a unit for {item}. The going rate is {ref}.",
		"{isk} for {item} at {station}. {pct} over what it trades for. I needed it.",
		"Whoever is selling {item} in {system} at {isk}: you're doing very well out of me.",
	)
	addLines(evMarketPaid, mktCombat, voiceAny,
		"{isk} a unit for {item}. Somebody thinks a war is coming.",
		"Paid {isk} for {item} because I'm out and the rats aren't waiting. {ref} last fortnight.",
		"Restocked {item} at {isk}. At this rate the ammunition costs more than the bounties pay.",
	)
	addLines(evMarketPaid, mktMining, voiceAny,
		"{isk} for a {item}. I could buy a used barge for what this hold of lasers cost.",
		"Paid {isk} for {item} in {system}. The belts don't pay that well.",
		"Paid {isk} for {item}. That's a week of Veldspar gone.",
		"[Bought|Picked up] {item} in {system} at {isk}. My barge had better be grateful.",
	)
	addLines(evMarketPaid, mktIndustry, voiceAny,
		"{item} at {isk} a unit. My build margin just went out the airlock.",
		"Bought {item} at {isk} — {pct} over the fortnight. Somebody's cornered it.",
		"Paid {isk} for {item}. [The job runs at a loss now.|There goes this month's margin.]",
		"{pct} over the odds for {item}. The line couldn't wait.",
	)
	addLines(evMarketPaid, mktTrade, voiceAny,
		"Paid {isk} for {item}. I'll be passing that on[, I'm afraid|. Sorry, everyone].",
		"{isk} for {item}. Somebody upstream knows I'm short.",
		"Paid {isk} for {item} to keep my own orders stocked. [Never again.|Thin month ahead.]",
	)
	addLines(evMarketPaid, motiveNone, voiceCorporate,
		"Acquired {item} at {isk}, {pct} above the regional average. The variance will be noted.",
	)
	addLines(evMarketPaid, motiveNone, voiceTerse,
		"{item}. {isk}. Robbery.",
	)
	addLines(evMarketPaid, motiveNone, voiceZealot,
		"{isk} for {item}. Usury is a sin, whoever is listing in {system}.",
	)

	// ── walked away ───────────────────────────────────────────────────────
	addLines(evMarketRefused, motiveNone, voiceAny,
		"{isk} for {item} at {station}? No. I'll go without.",
		"Not paying {isk} for {item}. It trades for {ref}.",
		"Somebody in {system} wants {isk} for {item}. They can keep it.",
	)
	addLines(evMarketRefused, mktCombat, voiceAny,
		"{isk} a unit for {item}? I'll fly with half a hold before I pay that.",
		"{isk} for {item}? [The rats can wait.|I'll make do with what's in the cargo.]",
		"Walked away from {item} at {isk}. The bounties don't cover that.",
	)
	addLines(evMarketRefused, mktMining, voiceAny,
		"{isk} for {item} in {system}. I'll mine with what I've got.",
		"Not paying {isk} for {item}. The old lasers will do another week.",
		"{item} at {isk} in {system}? [That's a full hold of Veldspar. No.|I'll haul one in from elsewhere.]",
	)
	addLines(evMarketRefused, motiveNone, voiceBrash,
		"{isk} for {item}? Whoever listed that has a sense of humour.",
	)
	addLines(evMarketRefused, motiveNone, voiceTribal,
		"{isk} for {item}. The Republic didn't free us so we could be robbed at the counter.",
	)

	// ── bought cheap ──────────────────────────────────────────────────────
	addLines(evMarketBargain, motiveNone, voiceAny,
		"Picked up {item} at {isk} in {station}. Under {ref} — somebody's clearing their hangar.",
		"{item} at {isk} in {system}. Cheapest I've seen in a while.",
		"Good price on {item} at {station} today, {isk}.",
	)
	addLines(evMarketBargain, mktTrade, voiceAny,
		"{item} at {isk}. That'll sell for more than that two systems over.",
		"Bought out the cheap {item} at {isk}. It'll be relisted by tonight.",
		"{item} at {isk}? [Somebody didn't check Jita.|Somebody didn't check the other hubs.]",
	)
	addLines(evMarketBargain, mktCombat, voiceAny,
		"{item} at {isk} in {station}. Filled the hold while it lasts.",
		"{item} at {isk}. Bought enough to last the month.",
		"Cheap {item} at {station}, {isk}. [The rats will get the benefit.|More shots per bounty.]",
	)
	addLines(evMarketBargain, motiveNone, voiceCorporate,
		"Procured {item} at {isk}, {pct} under the regional average. A sound purchase.",
	)

	// ── looking: a gouged ask ─────────────────────────────────────────────
	addLines(evMarketGouge, motiveNone, voiceAny,
		"Who is asking {isk} for {item} in {region}? It trades for {ref}.",
		"Cheapest {item} in {region} is {isk}. That's {pct} over the fortnight.",
		"Has anyone seen what {item} costs in {region} today? {isk}.",
	)
	addLines(evMarketGouge, mktCombat, voiceAny,
		"{item} at {isk} in {region}. Somebody bought up the ammunition and is waiting for a war.",
		"{isk} for {item}. I'll be shooting rats with harsh language.",
		"Who cornered {item} in {region}? {isk} a unit, {ref} last week.",
	)
	addLines(evMarketGouge, mktMining, voiceAny,
		"Who is asking {isk} for {item}? I could buy a used barge for that.",
		"{item} for {isk} in {region}. Somebody's mining the miners.",
		"{isk} for {item} in {region}. [At that price I'll mine with my bare hands.|A barge pilot can't afford to work.]",
	)
	addLines(evMarketGouge, mktIndustry, voiceAny,
		"{item} at {isk} in {region}. Nobody builds anything at that price.",
		"{isk} a unit for {item}? Every factory in {region} just stopped.",
		"Nobody in {region} is selling {item} under {isk}. The build waits.",
	)
	addLines(evMarketGouge, mktTrade, voiceAny,
		"{item} at {isk} in {region}, {pct} over normal. Somebody's haulers are late.",
		"{isk} for {item} in {region}. There's a margin in hauling some in.",
		"{item} is {pct} over in {region}. [Anyone with an empty industrial|Anyone with a spare hauler], there's your run.",
	)
	addLines(evMarketGouge, motiveNone, voiceZealot,
		"{isk} for {item}. Whoever set that price will answer for it.",
	)
	addLines(evMarketGouge, motiveNone, voiceShowman,
		"{isk} for {item}! Somebody in {region} is a comedian.",
	)
	addLines(evMarketGouge, motiveNone, voiceTerse,
		"{item}: {isk}. Absurd.",
	)

	// ── looking: the day against the fortnight ────────────────────────────
	addLines(evMarketClimb, motiveNone, voiceAny,
		"{item} has climbed {pct} in {region} today. Trading at {isk} against {ref}.",
		"Anyone know why {item} is up {pct} in {region}?",
		"{item} is running hot in {region} — {isk}, up from {ref}.",
	)
	addLines(evMarketClimb, mktCombat, voiceAny,
		"{item} is up {pct} in {region}. Somebody's arming for something.",
		"{item} up {pct}. [Must be a war on somewhere in {region}.|Somebody in {region} lost a fleet and is refitting.]",
		"{item} up {pct} in {region}. The bounties don't rise with it.",
	)
	addLines(evMarketClimb, mktMining, voiceAny,
		"{item} up {pct} today. Either the belts are busy or someone's buying them all.",
		"{item} is up {pct}. Somebody's been busy while I was in the belts.",
		"{item}, up {pct}. [Wish my ore went up like that.|The belts never pay like that.]",
	)
	addLines(evMarketClimb, mktIndustry, voiceAny,
		"{item} up {pct} in {region}. Every build sheet I have just changed.",
		"Watch {item} — {isk} today, {ref} over the fortnight.",
		"{item} at {isk} now, {ref} a fortnight back. [Glad I stockpiled.|Should have stockpiled.]",
	)
	addLines(evMarketClimb, mktTrade, voiceAny,
		"{item} is up {pct} in {region}. There's a run in that for somebody with a hold.",
		"{item} up {pct} in {region}. Whoever bought low last week is laughing.",
		"Watching {item} in {region}, up {pct}. [Might list mine.|Time to sell, I think.]",
	)
	addLines(evMarketSlump, motiveNone, voiceAny,
		"{item} has dropped {pct} in {region} today. Someone's dumping.",
		"{item} is down to {isk} in {region}. It was {ref} last week.",
		"Has the bottom fallen out of {item}? Down {pct} in {region}.",
	)
	addLines(evMarketSlump, mktCombat, voiceAny,
		"{item} down {pct} in {region}. Good time to stock up.",
		"{item} down {pct}. [Filling the hold tonight.|Buying while it's cheap.]",
		"Cheap {item} in {region} for once, down {pct}. Somebody's war must be over.",
	)
	addLines(evMarketSlump, mktMining, voiceAny,
		"{item} is cheap in {region} today — {isk}. Might be time for a spare.",
		"{item} down to {isk} in {region}. Hope that isn't a sign for ore prices.",
		"{isk} for {item} in {region}. [I'll hold mine till it recovers.|Not selling at that.]",
	)
	addLines(evMarketSlump, mktIndustry, voiceAny,
		"{item} down {pct} in {region}. Cheap inputs, for once.",
		"{item} at {isk}, against {ref}. Somebody's emptied a refinery.",
		"{item} has slid {pct} in {region}. [Time to queue more jobs.|Every line I have wants feeding at that price.]",
	)
	addLines(evMarketSlump, mktTrade, voiceAny,
		"{item} has fallen {pct} in {region}. Whoever's holding a hold of it is having a bad day.",
		"{item} down {pct} in {region}. [Glad I sold last week.|And I've a hangar full of it.]",
		"Down {pct} on {item} in {region}. Nobody haul any more in, please.",
	)

	// ── answers ───────────────────────────────────────────────────────────
	// General cells answer whatever was said; note cells answer an ad, a remark or a found seller.
	addLines(marketAnswerEvent(mktIntentPrice), motiveNone, voiceAny,
		"{isk} a unit, {target}.",
		"{isk} each, {target}. {station}.",
		"Last I looked, {isk} at {station}, {target}.",
		"{target}: {isk}[| a unit].",
	)
	addLines(marketAnswerEvent(mktIntentPrice), mktNoteAd, voiceAny,
		"{isk} a unit, {target}. {units} left at {station}.",
		"It's on the order, {target}: {isk} each at {station}.",
		"{isk} apiece. {total} if you take the lot.",
	)
	addLines(marketAnswerEvent(mktIntentPrice), mktNoteShop, voiceAny,
		"I've {units} {item} at {station}, {target}. {isk} each.",
		"{item}? {isk} a unit at {station}. {units} on the order.",
		"[Mine's|My order's] {isk} for {item}, {target}. {station}.",
	)
	addLines(marketAnswerEvent(mktIntentPrice), mktNoteTalk, voiceAny,
		"{isk}, {target}. Against {ref} last fortnight.",
		"I saw {isk}. It's usually nearer {ref}.",
		"{isk} when I looked, {target}. [Might be worse by now.|Could have moved since.]",
		"{ref} is the fair price, {target}. Today it's {isk}.",
	)

	addLines(marketAnswerEvent(mktIntentBuy), motiveNone, voiceAny,
		"It's on the market at {station}, {target}.",
		"The order's up at {station}. Help yourself.",
		"Check the orders at {station}, {target}.",
		"[Go ahead|By all means], {target}. It's all on the market.",
	)
	addLines(marketAnswerEvent(mktIntentBuy), mktNoteAd, voiceAny,
		"It's on the order at {station}, {target}. Buy as many as you like.",
		"Go ahead, {target} — {station}, {isk} each. First come.",
		"All yours, {target}. {isk} each at {station}, take what you need.",
		"[Glad to hear it|Good], {target}. The order at {station} won't last long.",
	)
	addLines(marketAnswerEvent(mktIntentBuy), mktNoteShop, voiceAny,
		"I've {units} {item} at {station}, {target}. {isk} a unit.",
		"{target}, there's {item} on my order at {station}. {isk} each.",
		"Got some. {units} {item} at {station}, {isk} apiece.",
	)
	addLines(marketAnswerEvent(mktIntentBuy), mktNoteTalk, voiceAny,
		"Not from me, {target}. I'm buying, not selling.",
		"If you find any under {isk}, tell me where.",
		"I'm looking myself, {target}. Best I found was {isk}.",
		"Wish I could help, {target}. [I haven't any to spare.|I'm short myself.]",
	)

	addLines(marketAnswerEvent(mktIntentHaggle), motiveNone, voiceAny,
		"The price is on the order, {target}. It's fair.",
		"Not today, {target}.",
		"[The price is the price|That's the price], {target}.",
		"Find it cheaper and I'll be surprised, {target}.",
	)
	addLines(marketAnswerEvent(mktIntentHaggle), mktNoteAd, voiceAny,
		"{isk} is already under what {item} trades for, {target}.",
		"The order's the order, {target}. {isk}.",
		"If it's still there next week, ask me again.",
	)
	addLines(marketAnswerEvent(mktIntentHaggle), mktNoteShop, voiceAny,
		"{isk} is the price, {target}. Somebody will pay it.",
		"{isk}, {target}. Buy it or don't.",
		"I'm not moving off {isk}, {target}. [Check the other orders if you like.|Nobody else in here is cheaper.]",
	)
	addLines(marketAnswerEvent(mktIntentHaggle), mktNoteTalk, voiceAny,
		"Tell that to whoever listed it, {target}.",
		"I don't set the prices, {target}. I just complain about them.",
		"Not my order, {target}. [I'm only the one grumbling.|Take it up with the seller.]",
	)
	addLines(marketAnswerEvent(mktIntentHaggle), motiveNone, voiceCorporate,
		"Pricing is not negotiable through Local, {target}.",
	)
	addLines(marketAnswerEvent(mktIntentHaggle), motiveNone, voicePirate,
		"You want a discount, {target}? I know where you undock.",
	)

	addLines(marketAnswerEvent(mktIntentAgree), motiveNone, voiceAny,
		"Isn't it, {target}.",
		"Glad it isn't just me.",
		"[Exactly|Quite], {target}.",
		"Somebody had to say it, {target}.",
	)
	addLines(marketAnswerEvent(mktIntentAgree), mktNoteTalk, voiceAny,
		"Somebody's sitting on the supply, {target}. Mark my words.",
		"Every week it's something. This week it's {item}.",
		"It was {ref} a fortnight ago, {target}. Look at it now.",
	)
	addLines(marketAnswerEvent(mktIntentAgree), mktNoteAd, voiceAny,
		"Thank you, {target}. Tell your friends.",
		"Glad somebody noticed, {target}.",
		"[It's a fair price|Fair price, isn't it], {target}. Help yourself.",
	)
	addLines(marketAnswerEvent(mktIntentAgree), mktNoteShop, voiceAny,
		"{isk} at {station}, {target}, while it lasts.",
		"Glad you think so, {target}. {isk} each.",
		"Better than you'll find elsewhere, {target}. [The order's at {station}.|{station}, whenever you're ready.]",
	)

	addLines(marketAnswerEvent(mktIntentMock), motiveNone, voiceAny,
		"Laugh all you like, {target}.",
		"You'd be surprised, {target}.",
		"[Funny|Very funny], {target}.",
		"We'll see who's laughing at the end of the month, {target}.",
	)
	addLines(marketAnswerEvent(mktIntentMock), mktNoteAd, voiceAny,
		"Somebody will pay it, {target}. Somebody always does.",
		"It'll sell, {target}. They always do.",
		"Your loss, {target}.",
	)
	addLines(marketAnswerEvent(mktIntentMock), mktNoteShop, voiceAny,
		"The order's at {station} whether you like it or not, {target}.",
		"Laugh away, {target}. It's still on sale at {station}.",
		"Nobody's forcing you to buy, {target}.",
	)
	addLines(marketAnswerEvent(mktIntentMock), mktNoteTalk, voiceAny,
		"It isn't funny when you need it, {target}.",
		"Laugh now, {target}. Wait till you need some.",
		"Check the market yourself, {target}. [Then laugh.|You'll stop laughing.]",
	)
	addLines(marketAnswerEvent(mktIntentMock), motiveNone, voiceBrash,
		"Keep laughing, {target}. You'll be buying from me next week.",
	)
	addLines(marketAnswerEvent(mktIntentMock), motiveNone, voiceCold,
		"Noted, {target}.",
	)
}
