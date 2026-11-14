package rolebots

// Summit lines: wars (summit_war.go). Plain text, setting vocabulary, no
// player slang. Slots per kind are listed in docs/SUMMIT_DESK.md; a line
// whose slot a beat cannot fill is never drawn.

func init() {
	// ── generic war titles: a thread opened by a beat other than the opener
	addSummit("war", summitPartTitle, voiceAny,
		"{sideA} and {sideB}: the war thread",
		"{sideA} vs {sideB}",
		"Re: the {sideA} and {sideB} business",
		"War: {sideA} against {sideB}",
		"{sideA} / {sideB} - what is actually going on",
		"So {sideA} and {sideB} are shooting each other now",
	)

	// ── war.declare: the declarer's CEO opens ──────────────────────────────
	addSummit("war.declare", summitPartTitle, voiceAny,
		"{us} declares war on {them}",
		"Formal declaration: {us} against {them}",
		"To {them}: you are at war",
		"[{usTicker}] is at war with [{themTicker}]",
		"Notice of hostilities, {us} and {them}",
		"{them}, consider this your warning",
	)
	addSummit("war.declare", summitPartTitle, voiceBrash, "Open season on {them}", "{them} had this coming")
	addSummit("war.declare", summitPartTitle, voiceZealot, "A judgement on {them}")
	addSummit("war.declare", summitPartTitle, voiceCorporate, "{us}: statement on hostilities with {them}")
	addSummit("war.declare", summitPartTitle, voicePirate, "{them}, your hangars look lonely")
	addSummit("war.declare", summitPartOpen, voiceAny,
		"As of today, {us} is at war with {them}. CONCORD has been paid and the bills are covered.",
		"We have filed with CONCORD. {us} and {them} are at war, effective now.",
		"This is the formal notice. {us} has declared war on {them}.",
		"For the record, and so nobody can claim they were not told: {us} is at war with {them}.",
	)
	addSummit("war.declare", summitPartOpen, voiceCold,
		"{them}. You are at war with {us}. Nothing more needs saying, but I will say a little.",
		"The paperwork is with CONCORD. From now on any {them} hull in space is a target.",
	)
	addSummit("war.declare", summitPartOpen, voiceBrash,
		"Well, {them}, you finally made enough noise. {us} is coming for you.",
		"Pour yourselves something strong, {them}. {us} just bought a war with your name on it.",
	)
	addSummit("war.declare", summitPartOpen, voiceTerse,
		"{us} is at war with {them}. Undock at your own risk.",
	)
	addSummit("war.declare", summitPartOpen, voiceZealot,
		"Scripture tells us that patience has an end. For {them}, it ends today. {us} is at war.",
		"We gave {them} every chance to turn from their path. They did not. {us} has declared war.",
	)
	addSummit("war.declare", summitPartOpen, voiceCorporate,
		"Following a review of our position, the board of {us} has authorised hostilities against {them}.",
		"{us} has entered a state of war with {them}. The decision was made on sound commercial grounds.",
	)
	addSummit("war.declare", summitPartOpen, voiceShowman,
		"Ladies and gentlemen of the Summit, a new show opens tonight: {us} against {them}.",
		"Every story needs a villain. {them} volunteered. {us} is at war.",
	)
	addSummit("war.declare", summitPartOpen, voiceTribal,
		"The elders of {us} have spoken and the debt is called in. We are at war with {them}.",
		"There is an old account between {us} and {them}. Today we settle it the old way.",
	)
	addSummit("war.declare", summitPartOpen, voicePirate,
		"Business is business. {us} is now at war with {them}, and business is good.",
		"{them}, you have ships and we have guns. CONCORD says we may now introduce them.",
	)
	addSummit("war.declare", summitPartFact, voiceAny,
		"We do this {motive}.",
		"Make no mistake about why: we do this {motive}.",
		"Anyone asking why can be told it is {motive}.",
		"There is a simple reason, and it is {motive}.",
		"Their pilots will be shot on sight in every system we fly.",
		"Our fleets are already in space.",
		"The terms of surrender are short and they know where to send them.",
		"We have set aside enough to keep this going for as long as it takes.",
		"Neutrals have nothing to fear from us. {them} has everything to fear.",
	)
	addSummit("war.declare", summitPartClose, voiceAny,
		"That is all.",
		"See you in space, {them}.",
		"Fly carefully. Or do not, it saves us time.",
		"Questions may be directed to our gunners.",
	)
	addSummit("war.declare", summitPartClose, voiceCold, "Do not reply. Undock.")
	addSummit("war.declare", summitPartClose, voiceBrash, "Bring friends. You will need them.")
	addSummit("war.declare", summitPartClose, voiceZealot, "May they find mercy in the next life. They will not find it here.")
	addSummit("war.declare", summitPartClose, voiceCorporate, "This statement is final and not open to negotiation.")
	addSummit("war.declare", summitPartClose, voiceTribal, "We remember. We always remember.")

	// ── war.defiant: the defender answers the declaration ────────────────
	addSummit("war.defiant", summitPartOpen, voiceAny,
		"We read your declaration, {them}. We are not impressed.",
		"So {them} wants a war. Fine. {us} is not going anywhere.",
		"Noted. {us} will be undocking exactly as often as before.",
		"Big words for an outfit that has yet to fire a shot.",
	)
	addSummit("war.defiant", summitPartOpen, voiceCold, "Received. Your pilots will be dealt with as they appear.")
	addSummit("war.defiant", summitPartOpen, voiceBrash, "Is that it? We expected a speech. Come on then, {them}.")
	addSummit("war.defiant", summitPartOpen, voiceTerse, "Noted. Undock.")
	addSummit("war.defiant", summitPartOpen, voiceZealot, "Those who raise a hand against the faithful will find it cut off. {us} stands.")
	addSummit("war.defiant", summitPartOpen, voiceCorporate, "{us} acknowledges the declaration. Our operations continue without interruption.")
	addSummit("war.defiant", summitPartOpen, voiceShowman, "Oh good, an audience. {them}, we will try not to make you look too bad.")
	addSummit("war.defiant", summitPartOpen, voiceTribal, "{them} forgets who we are. We will remind them.")
	addSummit("war.defiant", summitPartOpen, voicePirate, "You declared war on pirates. Think about that for a while, {them}.")
	addSummit("war.defiant", summitPartFact, voiceAny,
		"They say it is {motive}. They can say what they like.",
		"Every pilot of ours has been told. Fly in pairs, watch the gates.",
		"We have been at war before. We are still here.",
		"Their declaration fee will be the cheapest thing they lose this month.",
		"If they wanted a fight they could have asked. We would have said yes.",
	)
	addSummit("war.defiant", summitPartClose, voiceAny,
		"We will see who is still posting at the end of it.",
		"Bring your best, {them}.",
		"See you out there.",
	)

	// ── war.attacked: the defender opens ─────────────────────────────────
	addSummit("war.attacked", summitPartTitle, voiceAny,
		"{them} has declared war on {us}",
		"Unprovoked declaration by {them}",
		"Statement from {us} on the war with {them}",
		"Under attack by {them}",
		"Re: {them} and their war on us",
	)
	addSummit("war.attacked", summitPartTitle, voiceShowman, "{them} wants a war. Let us give them one.")
	addSummit("war.attacked", summitPartTitle, voiceZealot, "{them} raises arms against {us}")
	addSummit("war.attacked", summitPartOpen, voiceAny,
		"Today {them} filed a war declaration against {us}.",
		"Members and friends of {us}: as of today we are at war with {them}. We did not ask for it.",
		"{them} has decided that {us} is worth a CONCORD fee. Let us show them it was a poor investment.",
		"I want it on record that {us} did not start this. {them} did.",
	)
	addSummit("war.attacked", summitPartOpen, voiceCold, "{them} has declared on us. Their reasons are their own. Our answer is ours.")
	addSummit("war.attacked", summitPartOpen, voiceBrash, "Somebody at {them} got bored. {us} is happy to entertain.")
	addSummit("war.attacked", summitPartOpen, voiceTerse, "{them} declared on {us}. We fight.")
	addSummit("war.attacked", summitPartOpen, voiceCorporate, "{us} has been served a war declaration by {them}. We are reviewing our response.")
	addSummit("war.attacked", summitPartOpen, voiceTribal, "{them} has come for us. They will learn what our people learned long ago: we do not break.")
	addSummit("war.attacked", summitPartOpen, voicePirate, "{them} thinks we are an easy mark. Cute.")
	addSummit("war.attacked", summitPartFact, voiceAny,
		"Their stated reason is that they are at war with us {motive}. Make of that what you will.",
		"We will defend our space, our haulers and our miners.",
		"Every member has been told to fly in company until this is over.",
		"Any friend of ours who wants a share of the shooting knows where to find us.",
		"We are not looking for a fight, but we will not run from one.",
	)
	addSummit("war.attacked", summitPartClose, voiceAny,
		"Further updates in this thread.",
		"We will post the tally as it comes.",
		"Fly safe, everyone else.",
	)

	// ── war.justify: the declarer answers the defender ───────────────────
	addSummit("war.justify", summitPartOpen, voiceAny,
		"Unprovoked? {them} has a short memory.",
		"Since {them} has taken to the Summit, let me explain why {us} filed.",
		"Cry about it here if you like, {them}. It does not change the war.",
		"Our declaration stands, and so do our reasons.",
	)
	addSummit("war.justify", summitPartOpen, voiceCold, "We do not explain ourselves often. This once.")
	addSummit("war.justify", summitPartOpen, voiceBrash, "Ha. {them} came to the forum to complain. That went about as well as their last fleet.")
	addSummit("war.justify", summitPartOpen, voiceZealot, "Guilt does not need to be confessed to be real, {them}.")
	addSummit("war.justify", summitPartFact, voiceAny,
		"We went to war {motive}.",
		"They know what they did. Ask them.",
		"The surrender terms have not changed.",
		"Every day this goes on costs them more than it costs us.",
	)
	addSummit("war.justify", summitPartClose, voiceAny, "The war continues.", "Undock and talk to us there.")

	// ── war.gloat: a kill, from the killers ──────────────────────────────
	addSummit("war.gloat", summitPartOpen, voiceAny,
		"Another one for the board.",
		"A good day for {us}.",
		"{them} keeps undocking. We keep obliging.",
		"Scratch one {them} hull.",
		"Update from the front.",
	)
	addSummit("war.gloat", summitPartOpen, voiceCold, "Target destroyed.")
	addSummit("war.gloat", summitPartOpen, voiceBrash, "Did you see that? I did. Beautiful.", "Somebody tell {them} the war is still on.")
	addSummit("war.gloat", summitPartOpen, voiceTerse, "Kill.")
	addSummit("war.gloat", summitPartOpen, voiceZealot, "Another heretic brought to account.")
	addSummit("war.gloat", summitPartOpen, voiceCorporate, "A satisfactory result for {us} today.")
	addSummit("war.gloat", summitPartOpen, voiceShowman, "And the crowd goes wild.")
	addSummit("war.gloat", summitPartOpen, voiceTribal, "One more mark on the account.")
	addSummit("war.gloat", summitPartOpen, voicePirate, "Thank you for the donation, {them}.")
	addSummit("war.gloat", summitPartFact, voiceAny,
		"{victim} lost a {ship} in {system}.",
		"One {ship} belonging to {them} is now scrap in {system}.",
		"{victim} brought a {ship} to {system}. {victim} is now flying a capsule.",
		"That {ship} was worth {isk}. Was.",
		"{killer} landed the last shot.",
		"Call it {isk} off their books.",
		"The wreck is still warm in {system} if anyone wants to look.",
	)
	addSummit("war.gloat", summitPartClose, voiceAny,
		"Next.",
		"Keep them coming, {them}.",
		"Good hunting, everyone.",
		"Your move, {them}.",
	)

	// ── war.pod: a capsule, from the killers ─────────────────────────────
	addSummit("war.pod", summitPartOpen, voiceAny,
		"And the pod too.",
		"{victim} will be waking up in a clone bay.",
		"Not even the capsule made it out.",
	)
	addSummit("war.pod", summitPartFact, voiceAny,
		"{victim} of {them} was podded in {system}.",
		"Caught {victim}'s capsule in {system}. Hope the clone was up to date.",
		"{killer} took the pod in {system}.",
	)
	addSummit("war.pod", summitPartClose, voiceAny, "Medical bill is on us.", "Update your clones, {them}.")

	// ── war.vow: a loss, from the side that took it ──────────────────────
	addSummit("war.vow", summitPartOpen, voiceAny,
		"They got one.",
		"I lost my {ship} to {them} in {system}.",
		"Fair enough, {them} caught us out.",
		"Lost a ship. It happens. It will happen to them too.",
	)
	addSummit("war.vow", summitPartOpen, voiceCold, "Loss noted. Account open.")
	addSummit("war.vow", summitPartOpen, voiceBrash, "Enjoy it, {them}. It is the last one you get for free.")
	addSummit("war.vow", summitPartOpen, voiceTerse, "Lost a {ship}. Not the last word.")
	addSummit("war.vow", summitPartOpen, voiceZealot, "A trial. We are tested, and we endure.")
	addSummit("war.vow", summitPartOpen, voiceCorporate, "A {ship} has been written off. Replacement is already on order.")
	addSummit("war.vow", summitPartOpen, voiceTribal, "They took a ship from us. We have long memories and short tempers.")
	addSummit("war.vow", summitPartOpen, voicePirate, "Fine. You want to play rough, {them}. So do we.")
	addSummit("war.vow", summitPartFact, voiceAny,
		"{victim} went down in a {ship} in {system}.",
		"A {ship} worth {isk}, gone in {system}.",
		"They had the numbers in {system}. They will not always.",
		"We know who was there.",
		"The replacement hull is already being fitted.",
	)
	addSummit("war.vow", summitPartClose, voiceAny,
		"We will be back in {system}.",
		"This is not over.",
		"Remember this one, {them}. We will.",
	)

	// ── war.named: a capsuleer shot one of ours ──────────────────────────
	addSummit("war.named", summitPartOpen, voiceAny,
		"A word about {capsuleer}.",
		"{capsuleer} decided to join {them}'s war.",
		"For anyone keeping a list: {capsuleer}.",
		"We have a name.",
	)
	addSummit("war.named", summitPartOpen, voiceCold, "{capsuleer}. Remember the name. We will.")
	addSummit("war.named", summitPartOpen, voiceBrash, "Oh, {capsuleer}, you should not have done that.")
	addSummit("war.named", summitPartOpen, voiceZealot, "{capsuleer} has chosen a side, and it is the wrong one.")
	addSummit("war.named", summitPartOpen, voiceTribal, "{capsuleer} is in our book now.")
	addSummit("war.named", summitPartFact, voiceAny,
		"{capsuleer} was on the killmail when {victim} lost a {ship} in {system}.",
		"{capsuleer} of {capsuleerCorp} helped {them} kill {victim}'s {ship} in {system}.",
		"A {ship} worth {isk} went down in {system} and {capsuleer} had a hand in it.",
		"Every {us} pilot now knows the name {capsuleer}.",
		"We do not care whose war {capsuleer} thinks it is in. {capsuleer} is in ours now.",
	)
	addSummit("war.named", summitPartClose, voiceAny,
		"See you soon, {capsuleer}.",
		"Fly carefully, {capsuleer}.",
		"Nothing personal. Actually, it is.",
	)

	// ── war.tally / war.tallybad: the daily account ──────────────────────
	addSummit("war.tally", summitPartOpen, voiceAny,
		"Day {day} of the war with {them}.",
		"War update, day {day}.",
		"The numbers for day {day}.",
		"Since {them} keeps asking how it is going:",
	)
	addSummit("war.tally", summitPartOpen, voiceBrash, "Day {day} and {them} is still pretending this is close.")
	addSummit("war.tally", summitPartOpen, voiceCorporate, "Quarterly results came early. Day {day} summary follows.")
	addSummit("war.tally", summitPartOpen, voiceTerse, "Day {day}.")
	addSummit("war.tally", summitPartFact, voiceAny,
		"{us} has killed {ourKills} of theirs and lost {ourLosses}.",
		"Kills {ourKills}, losses {ourLosses}.",
		"{ourISK} of their ships destroyed so far.",
		"They have cost us {lostISK}. We have cost them {ourISK}.",
		"{ourKills} to {ourLosses}. Draw your own conclusions.",
	)
	addSummit("war.tally", summitPartClose, voiceAny, "More tomorrow.", "The war goes on.", "Same time tomorrow, {them}.")
	addSummit("war.tallybad", summitPartOpen, voiceAny,
		"Day {day}. Honest numbers, because somebody will post them anyway.",
		"Day {day} of this war.",
		"An update for our members, day {day}.",
	)
	addSummit("war.tallybad", summitPartOpen, voiceBrash, "Day {day}. Yes, yes, we have seen the board.")
	addSummit("war.tallybad", summitPartFact, voiceAny,
		"Kills {ourKills}, losses {ourLosses}. Not where we want it.",
		"{them} has the better of the ISK so far, {lostISK} against our {ourISK}.",
		"We have lost {ourLosses} hulls. Every one of them was replaced the same day.",
		"They are winning on paper. Wars are not fought on paper.",
	)
	addSummit("war.tallybad", summitPartClose, voiceAny, "We are not done.", "It turns. It always turns.")

	// ── war.quiet: nobody has fired in days ──────────────────────────────
	addSummit("war.quiet", summitPartOpen, voiceAny,
		"Is {them} still at war with us? Hard to tell.",
		"{days} without a shot fired. {them}, are you there?",
		"A quiet war is a cheap war, but this is getting silly.",
	)
	addSummit("war.quiet", summitPartOpen, voiceBrash, "Hello? {them}? We have been waiting {days}.")
	addSummit("war.quiet", summitPartOpen, voicePirate, "{days} and not one {them} hull in space. Smart. Boring, but smart.")
	addSummit("war.quiet", summitPartFact, voiceAny,
		"Our gunners are getting bored.",
		"Their station has never been so popular. Nobody leaves.",
		"We will keep paying the bills as long as they keep hiding.",
	)
	addSummit("war.quiet", summitPartClose, voiceAny, "Undock, {them}.", "We are still here.")

	// ── war.mutual ───────────────────────────────────────────────────────
	addSummit("war.mutual", summitPartOpen, voiceAny,
		"{us} has agreed to make this war mutual. No more bills, no way out.",
		"It is mutual now. Nobody gets to walk away from this one by stopping payment.",
		"We accepted. This war is mutual.",
	)
	addSummit("war.mutual", summitPartOpen, voiceBrash, "Mutual. Because we are having too much fun to stop.")
	addSummit("war.mutual", summitPartFact, voiceAny,
		"Neither side can retract now. It ends when one of us gives in.",
		"{them} wanted to settle this for good. So do we.",
	)
	addSummit("war.mutual", summitPartClose, voiceAny, "See you out there, {them}.")

	// ── war.claimwin / war.claimspin / war.claimquiet: the end ───────────
	addSummit("war.claimwin", summitPartOpen, voiceAny,
		"The war with {them} is over. {us} won it.",
		"So that is that. The war has {how}, and the board speaks for itself.",
		"{days} of war with {them}, and we are the ones posting about it.",
	)
	addSummit("war.claimwin", summitPartOpen, voiceBrash, "Game over, {them}. Thanks for playing.")
	addSummit("war.claimwin", summitPartOpen, voiceZealot, "Justice has been done. The war with {them} is over.")
	addSummit("war.claimwin", summitPartOpen, voiceCorporate, "{us} reports the successful conclusion of hostilities with {them}.")
	addSummit("war.claimwin", summitPartOpen, voiceTribal, "The account is settled. {them} paid it.")
	addSummit("war.claimwin", summitPartFact, voiceAny,
		"{ourKills} of theirs destroyed for {ourLosses} of ours.",
		"We killed {ourISK} of their ships. They managed {lostISK} of ours.",
		"The war {how} after {days}.",
		"Every pilot who flew in this one has our thanks.",
	)
	addSummit("war.claimwin", summitPartClose, voiceAny, "{them} knows where we are if they want another.", "Good fights, all.")
	addSummit("war.claimspin", summitPartOpen, voiceAny,
		"The war with {them} has {how}. We got what we came for.",
		"It is over. {us} is still standing, which is more than {them} hoped.",
		"A few words now that the war with {them} has {how}.",
	)
	addSummit("war.claimspin", summitPartOpen, voiceBrash, "Read the killboard if you want. We know who blinked.")
	addSummit("war.claimspin", summitPartOpen, voiceCorporate, "The objectives {us} set for this conflict have been met.")
	addSummit("war.claimspin", summitPartFact, voiceAny,
		"The numbers say {ourKills} to {ourLosses}. The numbers do not say who held the field.",
		"They spent {days} and a fortune to move us not one system.",
		"We lost ships. Ships are cheap. Pride is not.",
		"The war {how} after {days}, and we never stopped undocking.",
	)
	addSummit("war.claimspin", summitPartClose, voiceAny, "We will be ready for the next one.", "Onwards.")
	addSummit("war.claimquiet", summitPartOpen, voiceAny,
		"The war with {them} has {how}. Not one shot fired.",
		"{days} at war with {them} and nothing to show for it on either side.",
	)
	addSummit("war.claimquiet", summitPartFact, voiceAny,
		"They spent the whole war docked. We call that a win.",
		"A war nobody fought. CONCORD did well out of it, at least.",
	)
	addSummit("war.claimquiet", summitPartClose, voiceAny, "Moving on.")

	// ── war.observer / war.observerdull: an outsider's account ───────────
	addSummit("war.observer", summitPartOpen, voiceAny,
		"Since both of them are claiming victory, here are the actual numbers.",
		"I have been watching this one from the outside. For anyone who wants the real story:",
		"Both sides say they won. The killboard says something a little different.",
		"Neutral observer here. Neither of them will like this.",
	)
	addSummit("war.observer", summitPartOpen, voiceBrash, "Love the victory speeches. Now the board.")
	addSummit("war.observer", summitPartOpen, voiceCold, "The record, for whoever wants it.")
	addSummit("war.observer", summitPartFact, voiceAny,
		"{sideA} killed {aKills} and {sideB} killed {bKills}.",
		"In ISK, {sideA} destroyed {aISK} and {sideB} destroyed {bISK}.",
		"The war {how} after {days}.",
		"{sideA} paid CONCORD for the privilege, too.",
		"Draw your own conclusions about who won.",
	)
	addSummit("war.observer", summitPartClose, voiceAny, "Carry on.", "Good show, I suppose.", "Next war, please.")
	addSummit("war.observerdull", summitPartOpen, voiceAny,
		"I kept an eye on the {sideA} and {sideB} war. There was nothing to see.",
		"Most expensive staring contest of the year.",
	)
	addSummit("war.observerdull", summitPartFact, voiceAny,
		"{days} of war and not one kill on either side.",
		"The war {how} without a single loss. CONCORD was the only winner.",
	)
	addSummit("war.observerdull", summitPartClose, voiceAny, "Riveting.", "Wake me for the next one.")
}

// summitMottos are signature lines, by voice; voiceAny is shared.
var summitMottos = map[chatVoice][]string{
	voiceAny: {
		"Fly safe, fly smart.",
		"Undock with intent.",
		"Space is big. Our guns are bigger.",
		"Not lost, just exploring.",
		"Every hull is a loan.",
		"The gate is not your friend.",
	},
	voiceCold:      {"Quiet is not the same as harmless.", "Efficiency is its own reward.", "Nothing personal."},
	voiceBrash:     {"Loud and proud.", "Come and get it.", "I have never lost a fight I was paying attention to."},
	voiceTerse:     {"o7", "Undock.", "Fly."},
	voiceZealot:    {"Faith is a hull that does not breach.", "Glory to the Empire.", "In His name."},
	voiceCorporate: {"Profit is a discipline.", "Results, not excuses.", "Every contract honoured."},
	voiceShowman:   {"Life is short. Make it a good show.", "Freedom, flair, firepower.", "Always leave them wanting more."},
	voiceTribal:    {"We remember.", "The tribes endure.", "No chains, no masters."},
	voicePirate:    {"Your cargo, our pleasure.", "Pay up or blow up.", "No refunds."},
}
