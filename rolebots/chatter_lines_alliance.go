package rolebots

// Alliance channel lines (chatter_alliance.go). The speaker is a pilot of one member
// corporation talking to the others. Slots: {corp} the corporation the news is about (the
// speaker's own unless said otherwise), {othercorp} the other party, {system}, {where},
// {target} the other pilot as a link and {name} plain, {race}, {ship}, {isk}, {jumps}
// ("3 jumps"), {when} ("2h05m"), {rel} ("friend", "enemy"), {first} a first name, {item} a
// type link, {units}, {station}.
// Never {slur}: allies keep a floor of civility and chatSay drops the slot in an alliance
// channel. A cold reply is curt and distant, not an insult. No market shorthand, no talk of
// the game.

func init() {
	// ── a member under fire asks the alliance ─────────────────────────────
	addLines(evAllyHelp, motiveNone, voiceAny,
		"Alliance — I'm taking fire in {system} and my own wing is nowhere. Anyone close?",
		"{system}. {target} is on me. Any of you near?",
		"Need help in {system}, anyone. {target} in a {ship}.",
		"{corp} pilot under fire in {system}, near {where}. Anybody in range?",
		"Pinned in {system}. Our security's out of reach — is anyone in the alliance closer?",
		"{target} has me in {system}. Any friendly guns nearby?",
	)
	addLines(evAllyHelp, motiveNone, voiceTerse,
		"{system}. Under fire. Anyone?",
		"Help. {system}.",
	)
	addLines(evAllyHelp, motiveNone, voiceCorporate,
		"Requesting alliance assistance in {system}. Hostile {ship}, {target}.",
	)
	addLines(evAllyHelp, motiveNone, voiceZealot,
		"Brothers and sisters of the alliance — {system}. I am beset. Come if God puts you near.",
	)
	addLines(evAllyHelp, motiveNone, voiceTribal,
		"Allies, {system}. {target} is on me. If your clans are near, come.",
	)
	addLines(evAllyHelp, motiveNone, voiceShowman,
		"Anyone want a front-row seat? {system}, {target}, and I'm the one getting shot.",
	)

	// A member fleet really is there.
	addLines(evAllyHelpYes, motiveNone, voiceAny,
		"{target}, {corp} has a wing in {system} already. Hold on.",
		"We're in {system}, {target}. Coming to you.",
		"Hang in there, {target} — our fleet's working {system} right now.",
		"Copy, {target}. {corp}'s wing is on grid shortly.",
	)
	addLines(evAllyHelpYes, motiveNone, voiceTerse,
		"In {system}. Coming.",
	)
	addLines(evAllyHelpYes, motiveNone, voiceCorporate,
		"{corp} security is already deployed in {system}. Redirecting to you, {target}.",
	)
	addLines(evAllyHelpYes, motiveNone, voiceTribal,
		"We ride already in {system}, {target}. Hold.",
	)
	addLines(evAllyHelpYes, motiveNone, voiceZealot,
		"We are in {system}, {target}. Keep faith a little longer.",
	)
	// Nobody is.
	addLines(evAllyHelpNo, motiveNone, voiceAny,
		"Nobody of ours near {system}, {target}. Sorry.",
		"We're nowhere near {system}. Get out if you can, {target}.",
		"Can't reach {system} in time, {target}. Warp off.",
	)
	addLines(evAllyHelpNo, toneWarm, voiceAny,
		"Wish we could, {target}. Nobody of ours is close to {system}. Get clear and we'll talk.",
		"{target}, we're too far out. Fly safe — dock if you have to.",
		"[Sorry|Sorry, friend], {target}. We can't get to {system} in time. Get out alive.",
	)
	addLines(evAllyHelpNo, toneCold, voiceAny,
		"We've our own fires, {name}.",
		"Not from here, {name}. Warp out.",
		"Nobody to spare, {name}.",
	)
	addLines(evAllyHelpNo, motiveNone, voiceTerse,
		"Too far. Sorry.",
	)

	// ── fleet help calls ──────────────────────────────────────────────────
	addLines(evAllyCall, motiveNone, voiceAny,
		"{corp} needs ships in {system}. Any wing in range, come.",
		"Call for help from {corp}: {system}.",
		"{system}, everyone. {corp} [is asking for help|needs a hand].",
	)
	addLines(evAllyCall, motive(helpOutnumbered), voiceAny,
		"Our fleet in {system} is outnumbered. Any wing in range, come.",
		"{system}: we're badly outgunned. Help if you can.",
		"Too many of them in {system}. Anyone who can bring guns, bring them.",
	)
	addLines(evAllyCall, motive(helpRetreat), voiceAny,
		"We're pulling out of {system}. If you're close, cover us.",
		"{system} went bad. Falling back — anyone near, watch our backs.",
		"Withdrawing from {system}. Anyone close, keep the gate clear for us.",
	)
	addLines(evAllyCall, motive(helpSiege), voiceAny,
		"We're laying into a tower in {system}. More guns would finish it sooner.",
		"Siege in {system}. Bring something that hits hard.",
		"{system}: the tower's holding out. [Bring battleships.|Anything with big guns, come.]",
	)
	addLines(evAllyCall, motive(helpCallToArms), voiceAny,
		"Call to arms: {system}. {corp} needs every hull it can get.",
		"Every ship that can fly — {system}. {corp} calls the alliance to arms.",
		"[All hands|Everyone who can undock] — {system}. {corp} is calling the alliance in.",
	)
	addLines(evAllyCall, motive(helpCallToArms), voiceZealot,
		"To arms, all of you. {system}. God is watching who comes.",
	)
	addLines(evAllyCall, motive(helpCallToArms), voiceTribal,
		"Every clan in the alliance — {system}. Now.",
	)
	addLines(evAllyCall, motive(helpCallToArms), voiceCorporate,
		"All member assets to {system}. {corp} is formally calling the alliance to arms.",
	)

	// Another corporation's fleet took it up.
	addLines(evAllyCallOmw, motiveNone, voiceAny,
		"On our way to {system}, {corp}. {jumps} out.",
		"We've got your call, {corp}. Heading to {system}.",
		"Wing's turning for {system}. Hold on, {corp}.",
		"{corp}, we're coming to {system} — {jumps} away.",
		"Heard you, {corp}. {system} it is.",
	)
	addLines(evAllyCallOmw, motiveNone, voiceTerse,
		"Coming. {system}.",
	)
	addLines(evAllyCallOmw, motiveNone, voiceBrash,
		"Save some for us, {corp}. {system}, {jumps} out.",
	)
	addLines(evAllyCallOmw, motiveNone, voiceCorporate,
		"Reassigning our fleet to {system} in support of {corp}. Arrival in {jumps}.",
	)

	// ── a reinforced tower ────────────────────────────────────────────────
	addLines(evAllyDefend, motiveNone, voiceAny,
		"{corp}'s tower in {system} is reinforced. We need the alliance there.",
		"Our tower in {system} is in reinforcement. Comes out in {when}. Be there.",
		"{system}: they've put our tower into reinforcement. Every wing that can, form up there before {when} is up.",
		"Tower in {system} is under siege. We'll need help holding it.",
	)
	addLines(evAllyDefend, motiveNone, voiceCorporate,
		"Asset at risk: {corp}'s tower in {system}, reinforcement ends in {when}. Requesting alliance defence.",
	)
	addLines(evAllyDefend, motiveNone, voiceTribal,
		"They've come for our tower in {system}. Clans of the alliance, stand with us.",
	)
	addLines(evAllyDefendYes, motiveNone, voiceAny,
		"Our security wing is going to {system} for {corp}.",
		"We'll be at {system}, {corp}. Count us in.",
		"Sending ours to {system}. They won't take it easily.",
	)
	addLines(evAllyDefendYes, motiveNone, voiceTerse,
		"We'll be at {system}.",
	)
	addLines(evAllyDefendYes, motiveNone, voiceZealot,
		"We stand with {corp} at {system}.",
	)

	// ── a person asking ───────────────────────────────────────────────────
	addLines(evAllyHumanYes, motiveNone, voiceAny,
		"{first}, {corp} has a wing in {system} already. Hold on.",
		"Our fleet's working {system} right now, {first}. Hang on.",
		"{first} — we're in {system}. Coming to you.",
	)
	addLines(evAllyHumanNo, motiveNone, voiceAny,
		"Nearest wing of ours is {jumps} from {system} and on a job, {first}. Can't promise anything — get out if you can.",
		"{first}, the closest we have is {jumps} out. I wouldn't wait for it.",
		"We're {jumps} from {system}, {first}. Too far to count on. Dock up if you can.",
	)
	addLines(evAllyHumanNone, motiveNone, voiceAny,
		"Nobody of ours is out right now, {first}. Dock up if you can.",
		"No wing near {system}, {first}. Sorry — get clear.",
		"{first}, there's nobody flying near you. Warp to a station.",
	)

	// ── politics ──────────────────────────────────────────────────────────
	addLines(evAllyJoin, motiveNone, voiceAny,
		"{corp} has joined. Glad to be flying with you all.",
		"Hello, alliance. {corp} is in.",
		"{corp} reporting in. Good to be here.",
	)
	addLines(evAllyJoin, motiveNone, voiceCorporate,
		"{corp} has completed its accession. We look forward to a productive partnership.",
	)
	addLines(evAllyJoin, motiveNone, voiceTribal,
		"{corp} stands with the alliance now. Our clan, your clan.",
	)
	addLines(evAllyJoin, motiveNone, voiceZealot,
		"{corp} joins you. May we serve well together.",
	)
	addLines(evAllyJoin, motiveNone, voiceShowman,
		"{corp} has arrived. Try to contain your excitement.",
	)
	addLines(evAllyJoinBack, motiveNone, voiceAny,
		"Welcome, {corp}.",
		"Welcome aboard.",
		"Welcome in, {corp}. [Fly safe.|Mind the channel.]",
	)
	addLines(evAllyJoinBack, toneWarm, voiceAny,
		"Welcome, {corp}. Good to have you.",
		"Glad you're with us, {target}. Shout if you need anything.",
		"Welcome! {corp} will fit in fine.",
	)
	addLines(evAllyJoinBack, tonePolite, voiceAny,
		"Welcome aboard, {corp}.",
		"Noted. Welcome, {corp}.",
		"Glad to have {corp} with us.",
	)
	addLines(evAllyJoinBack, toneCold, voiceAny,
		"We'll see what {corp} are worth.",
		"{race} pilots in the alliance now. Fly straight and we'll get on.",
		"Welcome. Pull your weight.",
	)

	addLines(evAllyAccept, motiveNone, voiceAny,
		"Accepted {othercorp}'s application. They're with us once the paperwork clears.",
		"{othercorp} will be joining us. Make them welcome when they arrive.",
		"{othercorp}'s application is approved. Expect new faces in the channel.",
	)
	addLines(evAllyAcceptBack, motiveNone, voiceAny,
		"Good.",
		"Noted.",
		"Understood.",
	)
	addLines(evAllyAcceptBack, toneWarm, voiceAny,
		"Good news. More of us is better.",
		"Glad to hear it.",
		"[Good.|Excellent.] I'll say hello when they're in.",
	)
	addLines(evAllyAcceptBack, toneCold, voiceAny,
		"I hope you checked them.",
		"Was that wise?",
		"We'll see how they fly.",
	)

	addLines(evAllyLeave, motiveNone, voiceAny,
		"{corp} is leaving the alliance. It's been an honour.",
		"We're going our own way. Fly safe, all of you.",
		"{corp} is out. No hard feelings.",
	)
	addLines(evAllyLeave, motiveNone, voiceCorporate,
		"{corp} is withdrawing from the alliance. Our obligations end today.",
	)
	addLines(evAllyLeaveBack, motiveNone, voiceAny,
		"Fly safe, {corp}.",
		"o7",
		"Good luck, {corp}.",
	)
	addLines(evAllyLeaveBack, toneWarm, voiceAny,
		"Sorry to see you go, {corp}. The door stays open.",
		"That's a shame. Fly safe, {target}.",
		"[Sad day.|Shame.] Good luck out there, {corp}.",
	)
	addLines(evAllyLeaveBack, toneCold, voiceAny,
		"Good riddance.",
		"One less name on the bill.",
		"Their choice.",
	)

	addLines(evAllyKick, motiveNone, voiceAny,
		"{othercorp} is out of the alliance as of now.",
		"We've expelled {othercorp}. They no longer fly with us.",
		"{othercorp} has been removed from the alliance. Check your standings.",
	)
	addLines(evAllyKickBack, motiveNone, voiceAny,
		"Understood.",
		"Noted.",
		"Right.",
	)
	addLines(evAllyKickBack, toneWarm, voiceAny,
		"Hard thing to do. Fair, though.",
		"Shame it came to that.",
		"Not easy. [I'd have done the same.|You did right.]",
	)
	addLines(evAllyKickBack, toneCold, voiceAny,
		"Should have happened sooner.",
		"About time.",
		"Good.",
	)

	addLines(evAllyVote, motiveNone, voiceAny,
		"{corp} backs {othercorp} for the executor's chair.",
		"For the record, our vote goes to {othercorp}.",
		"We're putting our weight behind {othercorp} for executor.",
	)
	addLines(evAllyVoteBack, motiveNone, voiceAny,
		"Noted.",
		"Fair enough.",
		"We'll see how the votes fall.",
	)
	addLines(evAllyVoteBack, toneWarm, voiceAny,
		"Good choice. {othercorp} would do it well.",
		"We were thinking the same.",
		"{othercorp} has our vote too.",
	)
	addLines(evAllyVoteBack, toneCold, voiceAny,
		"{othercorp}? We'll be voting otherwise.",
		"Your vote. Not ours.",
		"We'd back someone else.",
	)

	addLines(evAllyExec, motiveNone, voiceAny,
		"{corp} has the chair now. Congratulations.",
		"New executor: {corp}.",
		"{corp} is executor now. [Good luck with it.|Big job.]",
	)
	addLines(evAllyExec, toneWarm, voiceAny,
		"Congratulations, {corp}. The chair's yours.",
		"Well deserved, {corp}.",
		"[Well done|Good on you], {corp}. Nobody better for it.",
	)
	addLines(evAllyExec, tonePolite, voiceAny,
		"{corp} holds the executor's chair. Congratulations.",
		"Congratulations to {corp} on the executorship.",
		"Our support to {corp} in the chair.",
	)
	addLines(evAllyExec, toneCold, voiceAny,
		"{corp} holds the chair. Let's see how long.",
		"So {corp} runs things now. We'll be watching.",
		"{corp}, then. Try not to waste it.",
	)
	addLines(evAllyExecBack, motiveNone, voiceAny,
		"Thank you. We'll try to deserve it.",
		"Thanks. The chair is everyone's, we just hold it.",
		"Appreciated. Let's get to work.",
	)
	addLines(evAllyExecBack, motiveNone, voiceCorporate,
		"Thank you. We will publish our priorities shortly.",
	)

	addLines(evAllyDues, motiveNone, voiceAny,
		"Paid our alliance dues. CONCORD gets fatter every week.",
		"Dues are in for {corp}.",
		"Another week, another bill to CONCORD.",
	)
	addLines(evAllyDues, motiveNone, voiceCorporate,
		"{corp}'s membership fee is settled for the period.",
	)
	addLines(evAllyDuesBack, motiveNone, voiceAny,
		"Same here.",
		"Paid ours too.",
		"Same every week.",
	)
	addLines(evAllyDuesBack, toneWarm, voiceAny,
		"It keeps the alliance on the books. Worth it.",
		"Small price for the company we keep.",
		"[Worth every ISK.|Money well spent.] Thanks for sorting it.",
	)
	addLines(evAllyDuesBack, toneCold, voiceAny,
		"Pay it and stop complaining.",
		"Everyone pays. No need to announce it.",
		"It's a bill. That's all.",
	)

	addLines(evAllyTithe, motiveNone, voiceAny,
		"Sent our share, {isk}, to the executor.",
		"{corp}'s tithe is paid: {isk}.",
		"There goes {isk}. Hope the fleets are worth it.",
	)
	addLines(evAllyTitheBack, motiveNone, voiceAny,
		"Received. It pays for the fleets.",
		"Thank you. It goes where it's needed.",
		"Got it. Every share counts.",
	)
	addLines(evAllyRefuse, motiveNone, voiceAny,
		"{corp} could not pay its share this time.",
		"{corp} is short on its {isk} share. We'll carry it for now.",
		"Still waiting on {corp}'s share.",
	)
	addLines(evAllyRefuseBack, motiveNone, voiceAny,
		"Treasury's thin. We'll make it good.",
		"We know. It's coming.",
		"Bad month. Give us time.",
	)
	addLines(evAllyRefuseBack, motiveNone, voiceCorporate,
		"Acknowledged. Payment will follow once our receivables clear.",
	)

	addLines(evAllyRel, motiveNone, voiceAny,
		"The alliance has set {othercorp} to {rel}.",
		"Standings update: {othercorp} [is now|has been set to] {rel}.",
		"Update your overview. The alliance has changed its standing toward {othercorp}.",
	)
	addLines(evAllyRel, whyFriendly, voiceAny,
		"The alliance has set {othercorp} to {rel}. Don't shoot them.",
		"{othercorp} are {rel} to the alliance now. Treat them that way.",
		"{othercorp} are {rel} now. [Mind your manners in Local.|Keep your guns off them.]",
	)
	addLines(evAllyRel, whyHostile, voiceAny,
		"{othercorp} are marked {rel} by the alliance. Act on it.",
		"The alliance names {othercorp} {rel}. Watch for them.",
		"{othercorp} are {rel} to us now. Don't let them near our towers.",
	)
	addLines(evAllyRel, whyWithdraw, voiceAny,
		"We've withdrawn our declaration on {othercorp}.",
		"{othercorp} are back to neutral for the alliance.",
		"{othercorp} are off the standings list. Treat them like anyone else.",
	)
	addLines(evAllyRelBack, motiveNone, voiceAny,
		"Noted.",
		"Overview's updated.",
		"Got it.",
	)
	addLines(evAllyRelBack, toneWarm, voiceAny,
		"Good call.",
		"Should have done it sooner.",
		"[Agreed.|Agreed entirely.] Makes sense.",
	)
	addLines(evAllyRelBack, toneCold, voiceAny,
		"Nobody asked us.",
		"If you say so.",
		"We'll live with it.",
	)

	addLines(evAllyWar, motiveNone, voiceAny,
		"War declared on {othercorp} in the alliance's name. Live in a day.",
		"The alliance is at war with {othercorp} from tomorrow.",
		"{othercorp} are a legal target for all of us as of tomorrow.",
	)
	addLines(evAllyWar, motiveNone, voiceZealot,
		"The alliance takes up arms against {othercorp}. Tomorrow we begin.",
	)
	addLines(evAllyWarBack, motiveNone, voiceAny,
		"Understood.",
		"[Right.|Right, then.] Stay sharp, everyone.",
		"Here we go again.",
	)
	addLines(evAllyWarBack, whyEager, voiceAny,
		"Finally.",
		"About time someone dealt with {othercorp}.",
		"We'll be ready.",
	)
	addLines(evAllyWarBack, whyWary, voiceAny,
		"I hope we know their numbers.",
		"Careful with {othercorp}. They bite.",
		"Fine. Nobody fly alone for a while.",
	)
	addLines(evAllyWarReq, motiveNone, voiceAny,
		"{corp} has asked the alliance for war on {othercorp}.",
		"We've put in a request for war on {othercorp}.",
		"{corp} wants war on {othercorp}. [The chair has it.|Waiting on the executor.]",
	)
	addLines(evAllyWarReqBack, motiveNone, voiceAny,
		"It's being weighed.",
		"We'll decide this week.",
		"Noted. The chair will look at it.",
	)
	addLines(evAllyWarHold, motiveNone, voiceAny,
		"The war requests can wait. Not this week.",
		"We're holding off on any new wars for now.",
		"No new declarations for a while. The treasury needs a rest.",
	)
	addLines(evAllyWarHoldBack, motiveNone, voiceAny,
		"Disappointing.",
		"Fair. We'd have liked it, though.",
		"Understood.",
	)

	addLines(evAllySovGain, motiveNone, voiceAny,
		"{system} is ours.",
		"We hold {system} now.",
		"Sovereignty of {system} is the alliance's.",
	)
	addLines(evAllySovGain, motiveNone, voiceZealot,
		"{system} is ours by God's grace.",
	)
	addLines(evAllySovGain, motiveNone, voiceTribal,
		"{system} belongs to the alliance now. Good.",
	)
	addLines(evAllySovLoss, motiveNone, voiceAny,
		"We've lost {system}.",
		"{system} isn't ours any more.",
		"{system} has fallen. [The sovereignty's gone.|We don't hold it now.]",
	)
	addLines(evAllySovBack, motiveNone, voiceAny,
		"Noted.",
		"[Right.|I see.] Thanks for the word.",
		"Understood.",
	)
	addLines(evAllySovBack, whyGain, voiceAny,
		"Good work, everyone.",
		"Now we have to hold it.",
		"Excellent.",
	)
	addLines(evAllySovBack, whyLoss, voiceAny,
		"We'll take it back.",
		"Painful. Which towers fell?",
		"Not for long.",
	)

	// ── ribbing ───────────────────────────────────────────────────────────
	addLines(evAllyLossBig, motiveNone, voiceAny,
		"That's a painful one, {name}. {isk} gone in {system}.",
		"Sorry for the {ship}, {corp}.",
		"Rough loss, {name}. A {ship} in {system} is no small thing.",
	)
	addLines(evAllyLossBig, toneWarm, voiceAny,
		"That's a painful one, {name}. Sorry. Shout if you need a ride home.",
		"{isk} in {system}? Ouch. Fly safe, {name}.",
		"Ouch, {name}. [Need a hand refitting?|Shout if you're short of ISK.]",
	)
	addLines(evAllyLossBig, toneCold, voiceAny,
		"Expensive way to learn, {corp}.",
		"{isk}. That'll hurt {corp}'s wallet.",
		"That's {isk} {corp} won't see again.",
	)
	addLines(evAllyLossSmall, motiveNone, voiceAny,
		"Lost a {ship} in {system}? Happens.",
		"{corp} down a {ship}. Fly safe.",
		"A {ship} down in {system}. [Unlucky.|Could be worse.]",
	)
	addLines(evAllyLossSmall, toneWarm, voiceAny,
		"A {ship}? You'll have another by supper, {name}.",
		"Only a {ship}, {name}. Get back out there.",
		"Cheap hull, {name}. [No harm done.|Insurance will cover most of it.]",
	)
	addLines(evAllyLossSmall, toneCold, voiceAny,
		"{corp} lose another {ship}. Again.",
		"Another {ship} from {corp}. Somebody keeps the insurers busy.",
		"Does {corp} buy the {ship} hulls in bulk?",
	)
	addLines(evAllyLossBack, motiveNone, voiceAny,
		"Thanks.",
		"It happens.",
		"[Fair.|Yeah.] Back in a new one soon.",
	)
	addLines(evAllyLossBack, toneCold, voiceAny,
		"Mind your own hangar.",
		"Noted.",
		"Thanks for nothing.",
	)
	addLines(evAllyKill, motiveNone, voiceAny,
		"Good kill, {corp}.",
		"Nice work, {name}.",
		"[Clean kill|Well done], {corp}.",
	)
	addLines(evAllyKill, toneWarm, voiceAny,
		"Nice work, {name}! Keep it up.",
		"Saw that, {corp}. Well flown.",
		"Good shooting, {name}. [Drinks are on {corp}.|That one'll be talked about.]",
	)
	addLines(evAllyKill, toneCold, voiceAny,
		"Even {corp} get lucky.",
		"One kill. Don't get used to it.",
		"Took {corp} long enough.",
	)
	addLines(evAllyKillBack, motiveNone, voiceAny,
		"Thanks.",
		"Cheers.",
		"They had it coming.",
	)

	// ── shop talk ─────────────────────────────────────────────────────────
	addLines(evAllyShopAsk, motiveNone, voiceAny,
		"Anyone in the alliance making {item}? The market wants too much.",
		"Does anybody here build {item}? I'm not paying what they're asking.",
		"Looking for {item} at a fair price. Any of our corporations make it?",
	)
	addLines(evAllyShopYes, motiveNone, voiceAny,
		"{corp} builds {item}. Ask us before you pay that.",
		"We make {item}. Talk to {corp} first.",
		"{corp} has {item} on the line. Send us a mail before you buy.",
	)
	addLines(evAllyShopYes, toneWarm, voiceAny,
		"We build {item} — for the alliance we'll do better than the market.",
		"Don't pay market for {item}. We'll make you some at cost.",
		"We build {item}. [Come see us first, friend.|Ask us first, we'll sort you out.]",
	)
	addLines(evAllyShopYes, toneCold, voiceAny,
		"{corp} makes {item}. Not cheaply, but cheaper than that.",
		"{corp} builds {item}. Ask us, but don't expect charity.",
		"We make {item}. Our price, not the market's.",
	)
	addLines(evAllyShopAd, motiveNone, voiceAny,
		"For the alliance: {units} of {item} at {isk}, {station}.",
		"Listed {item} at {station} — {units}, {isk} each. Allies welcome.",
		"Alliance price on {item}: {isk}, at {station}. {units} on the order.",
	)
	addLines(evAllyShopAdBack, motiveNone, voiceAny,
		"Noted.",
		"Might take you up on that.",
		"Good to know.",
	)
	addLines(evAllyShopAdBack, toneCold, voiceAny,
		"I'll check the market first.",
		"Maybe. If nobody's cheaper.",
		"I'll think about it.",
	)
	addLines(evAllyCareer, motiveNone, voiceAny,
		"Long week.",
		"Quiet week for {corp}.",
		"[Busy|Slow] week for {corp}. The usual.",
	)
	addLines(evAllyCareer, mktMining, voiceAny,
		"The belts are thin this week. Took all day to fill a hold.",
		"Somebody's been in our belts before us again.",
		"Good rock in the belts lately. Lasers haven't stopped.",
	)
	addLines(evAllyCareer, mktIndustry, voiceAny,
		"Every line we have is running. Minerals can't come in fast enough.",
		"Waiting on materials again. The factory floor's half empty.",
		"Research slots are full till next week.",
	)
	addLines(evAllyCareer, mktCombat, voiceAny,
		"The rats have been thick in our belts. Good bounties, though.",
		"Patrols have been quiet. Too quiet.",
		"Spent the week chasing rats. My guns need a rest.",
	)
	addLines(evAllyCareer, mktTrade, voiceAny,
		"Margins are thin this week. Everybody's hauling the same goods.",
		"Hauled half the region this week. My hold smells of it.",
		"Prices moved in our favour for once.",
	)
	addLines(evAllyCareerBack, motiveNone, voiceAny,
		"Same here.",
		"Could be worse.",
		"Same old.",
	)
	addLines(evAllyCareerBack, toneWarm, voiceAny,
		"Good to hear someone's doing well.",
		"If you need a hand, say so.",
		"Sounds like you've earned a quiet evening.",
	)
	addLines(evAllyCareerBack, toneCold, voiceAny,
		"Everyone has problems.",
		"Hm.",
		"Welcome to the club.",
	)
}
