package rolebots

// Summit lines: alliances and diplomacy (summit_alliance.go).

func init() {
	// ── alliance house thread ────────────────────────────────────────────
	addSummit("alliance", summitPartTitle, voiceAny,
		"{alliance}: news and members",
		"The {alliance} thread",
		"{alliance} [{allianceTicker}]",
		"What is going on in {alliance}",
	)
	addSummit("alliance.found", summitPartTitle, voiceAny,
		"Announcing {alliance}",
		"{alliance} [{allianceTicker}] is founded",
		"A new alliance: {alliance}",
		"{us} founds {alliance}",
	)
	addSummit("alliance.found", summitPartTitle, voiceZealot, "{alliance}: a covenant of the faithful")
	addSummit("alliance.found", summitPartTitle, voiceShowman, "Introducing {alliance}, coming to a system near you")
	addSummit("alliance.found", summitPartOpen, voiceAny,
		"Today {us} has founded {alliance}.",
		"After a long time talking about it, {alliance} is real. CONCORD has the paperwork.",
		"It is my pleasure to announce {alliance}, founded by {us}.",
	)
	addSummit("alliance.found", summitPartOpen, voiceCold, "{alliance} exists as of today. {us} holds the executor seat.")
	addSummit("alliance.found", summitPartOpen, voiceCorporate, "{us} is pleased to announce the formation of {alliance}, a strategic partnership open to like-minded corporations.")
	addSummit("alliance.found", summitPartOpen, voiceTribal, "The clans are stronger together. {us} has raised {alliance} as a banner for any who will stand under it.")
	addSummit("alliance.found", summitPartOpen, voicePirate, "Some of us got tired of sharing. Some of us got tired of not sharing. Say hello to {alliance}.")
	addSummit("alliance.found", summitPartFact, voiceAny,
		"The ticker is [{allianceTicker}]. Learn it.",
		"We are looking for corporations who can pull their own weight.",
		"Our friends will find us loyal. Our enemies will find us organised.",
		"Applications are open to any corporation that can show us what it does.",
		"{us} will hold the executor seat until the members decide otherwise.",
	)
	addSummit("alliance.found", summitPartClose, voiceAny, "Questions in this thread.", "More soon.", "Here is to {alliance}.")

	addSummit("alliance.doubt", summitPartOpen, voiceAny,
		"Another alliance. Give it a month.",
		"Good luck to {alliance}. I have watched a dozen of these come and go.",
		"So who exactly is in {alliance} apart from the founders?",
		"Nice ticker. Now do something with it.",
	)
	addSummit("alliance.doubt", summitPartFact, voiceAny,
		"Founding an alliance is easy. Holding one together when the first war comes is the hard part.",
		"Let us see who is still in {alliance} after the first round of dues.",
		"I will believe it when I see {alliance} fleets in space.",
	)
	addSummit("alliance.doubt", summitPartClose, voiceAny, "Prove me wrong.", "No offence meant. Well, a little.")

	addSummit("alliance.joined", summitPartOpen, voiceAny,
		"{us} has joined {alliance}.",
		"Happy to say {us} is now part of {alliance}.",
		"{us} flies under the {alliance} banner as of today.",
	)
	addSummit("alliance.joined", summitPartOpen, voiceCorporate, "{us} has completed its accession to {alliance}.")
	addSummit("alliance.joined", summitPartOpen, voiceTerse, "{us} is in {alliance}.")
	addSummit("alliance.joined", summitPartFact, voiceAny,
		"We look forward to flying with our new friends.",
		"Our pilots will be at the alliance's call.",
		"It was the right fit for us.",
		"Anyone who has a problem with {alliance} now has a problem with {us} too.",
	)
	addSummit("alliance.joined", summitPartClose, voiceAny, "o7 to the new neighbours.", "See you on the alliance channel.")

	addSummit("alliance.welcome", summitPartOpen, voiceAny,
		"Welcome aboard, {them}.",
		"Glad to have {them} with us.",
		"{them}, welcome to {alliance}.",
	)
	addSummit("alliance.welcome", summitPartFact, voiceAny,
		"The alliance channel is open, come and say hello.",
		"We have work for every pilot you can spare.",
		"Fly with us a while and you will see why we built this.",
	)
	addSummit("alliance.welcome", summitPartClose, voiceAny, "o7", "Good to have you.")

	addSummit("alliance.left", summitPartOpen, voiceAny,
		"{us} has left {alliance}.",
		"Time for {us} to go its own way. We have left {alliance}.",
		"After some thought, {us} is no longer a member of {alliance}.",
	)
	addSummit("alliance.left", summitPartOpen, voiceBrash, "{us} is out of {alliance}. No hard feelings. Some hard feelings.")
	addSummit("alliance.left", summitPartFact, voiceAny,
		"We thank the members for the time we flew together.",
		"It was not working for us any more.",
		"The reasons are our own business.",
		"Our standings toward the members stay as they are, for now.",
	)
	addSummit("alliance.left", summitPartClose, voiceAny, "Fly safe.", "Good luck to them.")

	addSummit("alliance.kick", summitPartOpen, voiceAny,
		"{them} is no longer a member of {alliance}.",
		"As executor, {us} has removed {them} from {alliance}.",
		"Effective today, {them} has been expelled from {alliance}.",
	)
	addSummit("alliance.kick", summitPartOpen, voiceCold, "{them} has been removed. The alliance is better for it.")
	addSummit("alliance.kick", summitPartFact, voiceAny,
		"The members were consulted.",
		"We do not carry corporations that will not carry themselves.",
		"Our reasons have been given to {them} directly.",
		"Membership in {alliance} is a commitment, not a decoration.",
	)
	addSummit("alliance.kick", summitPartClose, voiceAny, "That is all on the matter.", "We wish them well. Elsewhere.")

	addSummit("alliance.kicked", summitPartOpen, voiceAny,
		"So {them} has thrown us out of {alliance}. Let us talk about that.",
		"For the record, {us} did not leave {alliance}. We were pushed.",
		"{them} decided {us} was not good enough for {alliance}. Their loss.",
	)
	addSummit("alliance.kicked", summitPartOpen, voiceBrash, "Kicked out by {them}. Best thing that ever happened to us.")
	addSummit("alliance.kicked", summitPartFact, voiceAny,
		"We paid our dues and flew every op.",
		"Look at who runs {alliance} and you will understand.",
		"We will manage just fine on our own.",
		"Anyone else in {alliance} should ask themselves who is next.",
	)
	addSummit("alliance.kicked", summitPartClose, voiceAny, "We will remember this, {them}.", "Moving on.")

	addSummit("alliance.executor", summitPartOpen, voiceAny,
		"{us} now holds the executor seat of {alliance}.",
		"The members have spoken: {us} is executor of {alliance}.",
		"A change at the top of {alliance}. {us} takes the executor seat.",
	)
	addSummit("alliance.executor", summitPartFact, voiceAny,
		"We thank the members for their trust.",
		"Nothing changes for our friends. Some things change for our enemies.",
		"The first order of business is the alliance's standing with its neighbours.",
	)
	addSummit("alliance.executor", summitPartClose, voiceAny, "Onwards.", "o7, {alliance}.")

	addSummit("alliance.enemy", summitPartOpen, voiceAny,
		"{alliance} now lists {them} as {standing}.",
		"A notice from {alliance} regarding {them}.",
		"{them} is now formally {standing} to {alliance}.",
	)
	addSummit("alliance.enemy", summitPartOpen, voiceZealot, "{them} has made itself an enemy of {alliance}. So be it.")
	addSummit("alliance.enemy", summitPartFact, voiceAny,
		"Our members have been told what that means.",
		"They know what they did.",
		"We do not expect them to like it.",
	)
	addSummit("alliance.enemy", summitPartClose, voiceAny, "Act accordingly.", "That is all.")
	addSummit("alliance.friend", summitPartOpen, voiceAny,
		"{alliance} now counts {them} as a {standing}.",
		"Good news for {alliance} and {them}: we are on {standing} terms.",
	)
	addSummit("alliance.friend", summitPartFact, voiceAny,
		"Our pilots will treat theirs accordingly.",
		"It has been a long time coming.",
	)
	addSummit("alliance.friend", summitPartClose, voiceAny, "o7 to {them}.")

	// ── pacts between two corporations ───────────────────────────────────
	addSummit("pact", summitPartTitle, voiceAny,
		"{us} and {them}",
		"Diplomacy: {us} and {them}",
		"[{usTicker}] and [{themTicker}]",
	)
	addSummit("pact.signed", summitPartTitle, voiceAny,
		"{us} and {them} sign {pact}",
		"An agreement between {us} and {them}",
		"Notice of {pact}: {us} and {them}",
	)
	addSummit("pact.signed", summitPartOpen, voiceAny,
		"{us} and {them} have agreed to {pact}.",
		"Some good news for a change: {us} and {them} have signed {pact}.",
		"Pleased to announce {pact} between {us} and {them}.",
	)
	addSummit("pact.signed", summitPartOpen, voiceCorporate, "{us} has concluded {pact} with {them}. Terms are standard and binding.")
	addSummit("pact.signed", summitPartFact, voiceAny,
		"Our pilots will not fire on theirs.",
		"Both sides have given their word. We intend to keep ours.",
		"It took a few letters and one envoy who earned the fee.",
	)
	addSummit("pact.signed", summitPartClose, voiceAny, "o7 to {them}.", "Here is to a long one.")
	addSummit("pact.ack", summitPartOpen, voiceAny,
		"Confirmed from our side. {us} stands by {pact} with {them}.",
		"{us} agrees. The pact holds.",
		"Signed and sealed on our end.",
	)
	addSummit("pact.ack", summitPartFact, voiceAny,
		"We will honour it as long as they do.",
		"Our members have been told.",
	)
	addSummit("pact.ack", summitPartClose, voiceAny, "o7")

	addSummit("pact.peace", summitPartTitle, voiceAny, "Peace between {us} and {them}", "{us} and {them} make peace")
	addSummit("pact.peace", summitPartOpen, voiceAny,
		"{us} and {them} have made peace.",
		"It is over between {us} and {them}. Peace has been agreed.",
		"Enough ships have burned. {us} has made peace with {them}.",
	)
	addSummit("pact.peace", summitPartFact, voiceAny,
		"Any war between us is being withdrawn.",
		"We do not expect to be friends. We expect not to shoot each other.",
		"Both sides lost more than either will admit.",
	)
	addSummit("pact.peace", summitPartClose, voiceAny, "Fly safe.", "So be it.")
	addSummit("pact.peaceack", summitPartOpen, voiceAny,
		"Confirmed. {us} is at peace with {them}.",
		"Yes, it is true. We have made peace with {them}.",
	)
	addSummit("pact.peaceack", summitPartFact, voiceAny,
		"We will keep our side of it.",
		"Nobody won. Everybody is tired.",
	)
	addSummit("pact.peaceack", summitPartClose, voiceAny, "o7")

	addSummit("pact.reparation", summitPartTitle, voiceAny, "{us} pays reparations to {them}")
	addSummit("pact.reparation", summitPartOpen, voiceAny,
		"{us} has paid reparations to {them}.",
		"To settle the matter, {us} has paid {them} {isk}.",
	)
	addSummit("pact.reparation", summitPartFact, voiceAny,
		"We consider the account closed.",
		"It was the right thing to do, and it was cheaper than the alternative.",
	)
	addSummit("pact.reparation", summitPartClose, voiceAny, "Matter closed.")
	addSummit("pact.repack", summitPartOpen, voiceAny,
		"Received, {them}. The account is settled.",
		"{us} confirms receipt of reparations from {them}.",
	)
	addSummit("pact.repack", summitPartFact, voiceAny,
		"We will not forget why it had to be paid.",
		"Our members can stand down.",
	)
	addSummit("pact.repack", summitPartClose, voiceAny, "o7")

	// ── a broken pact: its own thread ────────────────────────────────────
	addSummit("pact.broken", summitPartTitle, voiceAny,
		"{them} broke its word to {us}",
		"Betrayed by {them}",
		"A warning about {them}",
		"{them} cannot be trusted",
	)
	addSummit("pact.broken", summitPartOpen, voiceAny,
		"We had a pact with {them}. They broke it.",
		"Anyone thinking of signing anything with {them}: read this first.",
		"{them} gave us their word. It turns out their word is worth nothing.",
	)
	addSummit("pact.broken", summitPartOpen, voiceCold, "{them} broke our pact. Consider this the receipt.")
	addSummit("pact.broken", summitPartOpen, voiceZealot, "The oath-breaker will find no shelter. {them} has broken faith with {us}.")
	addSummit("pact.broken", summitPartOpen, voiceTribal, "{them} broke the pact. Among our people that means one thing.")
	addSummit("pact.broken", summitPartFact, voiceAny,
		"Their pilots opened fire on ours while the pact was standing.",
		"We kept every term. They did not.",
		"Whatever happens next, they started it.",
		"Every corporation that deals with {them} should remember this day.",
	)
	addSummit("pact.broken", summitPartClose, voiceAny, "We will answer this.", "You have been warned.")
	addSummit("pact.excuse", summitPartOpen, voiceAny,
		"Before everyone believes {them}, here is our side of it.",
		"{them} is making a lot of noise for an outfit that broke the terms first.",
		"Betrayal is a strong word, {them}.",
	)
	addSummit("pact.excuse", summitPartOpen, voiceBrash, "Oh no, we shot at {them}. Anyway.")
	addSummit("pact.excuse", summitPartFact, voiceAny,
		"The pact was dead long before anyone fired a shot.",
		"They know what they did to make this happen.",
		"We are not going to apologise for defending ourselves.",
	)
	addSummit("pact.excuse", summitPartClose, voiceAny, "That is all we will say.", "Believe what you like.")
	addSummit("pact.observer", summitPartTitle, voiceAny,
		"{breaker} and {wronged}: so much for that pact",
		"The {wronged} and {breaker} falling-out",
	)
	addSummit("pact.observer", summitPartOpen, voiceAny,
		"So much for {breaker} and {wronged}'s pact.",
		"Remember when {wronged} and {breaker} were friends? That was nice.",
		"Pacts in this part of space last about as long as the ink.",
	)
	addSummit("pact.observer", summitPartFact, voiceAny,
		"Whoever started it, {breaker} is going to find people less keen to sign with them.",
		"I give it a week before there is a war declaration in here.",
		"Lesson for everyone: get it in writing, then do not believe the writing.",
	)
	addSummit("pact.observer", summitPartClose, voiceAny, "Popcorn ready.", "Carry on.")
}
