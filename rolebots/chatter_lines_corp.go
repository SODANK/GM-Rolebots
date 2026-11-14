package rolebots

// Corp chat lines (chatter_corp.go). An order line's motive cell is the department
// ("operations", "security"), so directors of different arms do not sound alike.
// Slots: {member} the pilot called out and {head} the director (both links), {title}, {dept}
// the arm's name, {system}, {ore} ("Veldspar, Scordite and Pyroxeres"), {reason} the desk's
// own reason, read as an aside ("the factory at Rens is short of 3 mineral(s)").
// A help call: {target} is the attacker, {system}, {where}, {ship} their hull.
// The answer: {target} is the member who called.

const (
	deptOps      = motive(portOperations)
	deptSecurity = motive(portSecurity)
)

func init() {
	// ── a director calls a member out ─────────────────────────────────────
	addLines(evCorpOrder, deptOps, voiceAny,
		"{member}, take a barge out for {ore}. {reason}.",
		"{member} — mining op. We need {ore}. {reason}.",
		"{member}, I need {ore} by the end of the day.",
		"Mining wing: {ore}. {member}, you're on it.",
		"{member}, get your lasers on {ore}. The factory's starving.",
		"Ops board updated: {ore}. {member}, lead it.",
		"{member}, {ore}, as much as your hold takes. {reason}.",
		"{member}, stop whatever you're doing. {ore}. Now.",
	)
	addLines(evCorpOrder, deptOps, voiceCorporate,
		"{member}, quota revision: {ore}. {reason}.",
		"{member}, the production schedule needs {ore}. Deliver.",
	)
	addLines(evCorpOrder, deptOps, voiceZealot,
		"{member}, God gives us the rock. Bring me {ore}.",
	)
	addLines(evCorpOrder, deptOps, voiceTribal,
		"{member}, the clan needs {ore}. Go.",
	)
	addLines(evCorpOrder, deptSecurity, voiceAny,
		"{member}, form up and get to {system}. {reason}.",
		"Combat wing to {system}. {member}, you're point.",
		"{member}, {system}. {reason}. Go.",
		"Security tasking: {system}. {member}, take your wing.",
		"{member}, I want ships in {system} within the hour.",
		"{member}, patrol {system}. Shoot anything that looks at us wrong.",
		"Heads up, wing. {system}. {member}, lead it out. {reason}.",
	)
	addLines(evCorpOrder, deptSecurity, voiceCold,
		"{member}. {system}. Now.",
	)
	addLines(evCorpOrder, deptSecurity, voiceBrash,
		"{member}! Grab the guns, we're going to {system}.",
	)
	addLines(evCorpOrder, deptSecurity, voiceCorporate,
		"{member}, asset protection required in {system}. {reason}.",
	)
	addLines(evCorpOrder, deptSecurity, voiceZealot,
		"{member}, take the faithful to {system}. {reason}.",
	)
	addLines(evCorpOrder, deptSecurity, voiceTribal,
		"{member}, the clan rides for {system}.",
	)

	// ── the member answers ────────────────────────────────────────────────
	addLines(evCorpAck, motiveNone, voiceAny,
		"On it, {head}.",
		"Copy, {head}. Undocking.",
		"Understood.",
		"Will do.",
		"Aye, {head}.",
		"Roger. Heading out.",
		"On my way.",
		"Consider it done, {head}.",
	)
	addLines(evCorpAck, deptOps, voiceAny,
		"Barge is fuelled. Going for {ore}.",
		"{ore}, got it. Where's the hauler?",
		"Fine, {ore}. Somebody bring a can.",
	)
	addLines(evCorpAck, deptSecurity, voiceAny,
		"Wing's forming. {system} in ten.",
		"{system}, copy. Guns hot.",
		"Moving to {system}.",
	)
	addLines(evCorpAck, motiveNone, voiceTerse,
		"k.",
		"o7",
		"Yep.",
	)
	addLines(evCorpAck, motiveNone, voiceCorporate,
		"Acknowledged, {head}. Logging the task.",
	)
	addLines(evCorpAck, motiveNone, voiceZealot,
		"As God wills, {head}.",
	)
	addLines(evCorpAck, motiveNone, voiceTribal,
		"For the clan, {head}.",
	)
	addLines(evCorpAck, motiveNone, voiceShowman,
		"Say no more, {head}. I'm on stage.",
	)
	addLines(evCorpAck, motiveNone, voiceBrash,
		"Finally, something to do.",
	)

	// ... less graciously
	addLines(evCorpGrumble, motiveNone, voiceAny,
		"Again? Fine.",
		"I just docked, {head}.",
		"Why is it always me?",
		"Can someone else take this one?",
		"Ugh. Going.",
		"{head}, do you know what time it is?",
		"Last time I did this I lost a ship.",
	)
	addLines(evCorpGrumble, deptOps, voiceAny,
		"{ore}? Again? My barge hates {ore}.",
		"Sure, I'll just mine the whole belt myself.",
		"More {ore}? [My hold still smells of the last lot.|The belts are picked clean.]",
	)
	addLines(evCorpGrumble, deptSecurity, voiceAny,
		"{system}? That's a death trap.",
		"Who's paying for my ship when this goes wrong?",
		"{system} again? Fine. Somebody bring a spare ship.",
	)

	// ── the director's last word ──────────────────────────────────────────
	addLines(evCorpFollow, motive(evCorpAck), voiceAny,
		"Good.",
		"Thanks.",
		"Fly safe.",
		"Report back.",
		"Knew I could count on you.",
	)
	addLines(evCorpFollow, motive(evCorpGrumble), voiceAny,
		"Don't make me ask twice.",
		"You'll live.",
		"Less talking, more flying.",
		"Noted. Now go.",
		"There's a reason it's you. You're good at it.",
		"Complaints go to HR. HR is also me. Go.",
	)

	// ── a member under fire ───────────────────────────────────────────────
	addLines(evCorpHelp, motiveNone, voiceAny,
		"Taking fire at {where} in {system}! {target} in a {ship}.",
		"Need help in {system}. {target} is on me.",
		"Hostile in {system} — {target}. Anyone close?",
		"{target} jumped me at {where}. {system}.",
		"Under attack in {system}. Can anyone come?",
		"Help! {system}, {target}.",
		"{system}: {target} has me pointed.",
	)
	addLines(evCorpHelp, motiveNone, voiceTerse,
		"{system}. {target}. Help.",
	)
	addLines(evCorpHelp, motiveNone, voiceCorporate,
		"Requesting security response in {system}. Aggressor: {target}.",
	)
	addLines(evCorpHelp, motiveNone, voiceBrash,
		"Some clown named {target} is shooting me in {system}. Who wants a kill?",
	)

	addLines(evCorpHelpYes, motiveNone, voiceAny,
		"Hold on, {target}. We're coming.",
		"Wing's on the way. Hang in there.",
		"Burning to {system} now.",
		"Keep them busy, {target}. Two jumps.",
		"On it. Don't die before we get there.",
		"Security responding to {system}.",
	)
	addLines(evCorpHelpYes, motiveNone, voiceCold,
		"Inbound. Hold.",
	)
	addLines(evCorpHelpYes, motiveNone, voiceBrash,
		"Oh, they picked a fight with US? Coming.",
	)
	addLines(evCorpHelpNo, motiveNone, voiceAny,
		"Nobody close, {target}. Warp out.",
		"Can't reach {system} in time. Get out of there.",
		"Wing's tied up. Dock up, {target}.",
		"You're on your own, {target}. Sorry.",
		"Too far. Burn for the gate.",
		"Warp to a station, {target}. We can't cover you.",
	)
	addLines(evCorpHelpNo, motiveNone, voiceCold,
		"No support available. Extract.",
	)

	// Reply scheduled by chatterPass after the pers.loss line; {target} is the member who lost it.
	// schedules after it. {target} is the member who lost it.
	addLines(evCorpConsole, motiveNone, voiceAny,
		"Unlucky, {target}.",
		"o7 {target}. Insurance will cover it.",
		"Who was it? We'll remember.",
		"That's the third one this week, {target}.",
		"Get back to station, there's a spare in the hangar.",
		"Rip. Want me to bring you a ship?",
		"gf anyway, {target}.",
		"Again, {target}? Really?",
		"We'll get them back.",
	)
	addLines(evCorpConsole, motiveNone, voiceBrash,
		"Lol, {target}. Fit a tank next time.",
	)
	addLines(evCorpConsole, motiveNone, voiceCorporate,
		"Loss logged, {target}. Replacement request approved.",
	)

	// ── work off the board ────────────────────────────────────────────────
	// General cells are kind-neutral: {work} comes from workWords, {item} is the product or the print.
	addLines(evCorpClaim, motiveNone, voiceAny,
		"I'll take the {item} order — {work}.",
		"Got the corporation's {item} job. {work}.",
		"{item} for the corporation: {work}. Mine.",
		"Taking the {item} order at {station}.",
	)
	addLines(evCorpClaim, workMotive(workKindBuild), voiceAny,
		"Grabbing {n} runs of {item} for the corporation.",
		"Putting the corporation's {item} on my line. {work}.",
		"{item} off the board, {work}.",
	)
	addLines(evCorpClaim, workMotive(workKindME), voiceAny,
		"I'll put the corporation's {item} through material research. {work}.",
		"Taking the {item} research order — {work}.",
		"[Booked a lab for|I've a lab slot for] the {item} print: {work}.",
		"The {item} wastes too much. I'll run {work} on it.",
	)
	addLines(evCorpClaim, workMotive(workKindPE), voiceAny,
		"I'll take the time research on the corporation's {item}. {work}.",
		"{item}: time research for the corporation, {work}.",
		"The {item} builds too slowly. [I'll fix that|Putting it in a lab]: {work}.",
	)
	addLines(evCorpClaim, workMotive(workKindCopy), voiceAny,
		"I'll copy the corporation's {item}. {work}.",
		"Taking the {item} copy order — {work}.",
		"Running {work} of the {item} for the library.",
	)
	addLines(evCorpClaimOK, motiveNone, voiceAny,
		"Thanks, {member}.",
		"Good, {member}. That one's overdue.",
		"Noted, {member}.",
	)
	addLines(evCorpClaimOK, workMotive(workKindBuild), voiceAny,
		"{member}, don't let the {item} sit in the hangar after.",
		"Good, {member}. We need that {item} [this week|soon].",
		"Thanks, {member}. Put the {item} in the corporation hangar when it's done.",
	)
	addLines(evCorpClaimOK, workMotive(workKindCopy), voiceAny,
		"{member}, the copies go back to the library when they're done.",
		"Good, {member}. [The library's thin.|We're short of copies.]",
		"{member}, label them properly this time.",
	)
	addLines(evCorpClaimOK, workMotive(workKindME), voiceAny,
		"{member}, bring the print back to the library after.",
		"Don't rush it, {member}. The research pays for itself.",
		"Thanks, {member}. [That print needs it.|About time someone did.]",
	)
}

// workMotive is the motive cell a work order's lines live in.
func workMotive(kind string) motive { return motive("work." + kind) }

// More corp chat, and the voices for it.
func init() {
	addLines(evCorpOrder, deptOps, voiceAny,
		"{member}, I've got a hauler waiting on {ore}. Don't make it wait.",
		"{member}, factory's dry. {ore}, please.",
		"Who's free? {member} — {ore}, now.",
	)
	addLines(evCorpOrder, deptOps, voiceCold,
		"{member}. {ore}. Today.",
	)
	addLines(evCorpOrder, deptOps, voiceBrash,
		"Rise and shine, {member}! {ore} won't mine itself.",
	)
	addLines(evCorpOrder, deptOps, voiceShowman,
		"{member}, the stage is a belt and the star is {ore}. Go.",
	)
	addLines(evCorpOrder, deptSecurity, voiceAny,
		"{member}, we've got trouble in {system}. Go sort it.",
		"{member}, take two hulls and hold {system}.",
		"{member}, sweep {system}. Report anything with a pulse.",
	)
	addLines(evCorpOrder, deptSecurity, voiceShowman,
		"{member}, {system} needs a hero. Audition starts now.",
	)

	addLines(evCorpAck, motiveNone, voiceAny,
		"Already undocking.",
		"Copy all.",
		"Yes, {head}.",
		"Leaving now.",
	)
	addLines(evCorpAck, deptOps, voiceAny,
		"Lasers warm. {ore} incoming.",
		"I'll fill a hold of {ore}.",
	)
	addLines(evCorpAck, deptSecurity, voiceAny,
		"Guns up, heading to {system}.",
		"{system}. I'll bring friends.",
	)
	addLines(evCorpGrumble, motiveNone, voiceAny,
		"Do I get paid extra for this?",
		"Sure. Not like I had plans.",
		"Okay okay. Going.",
	)
	addLines(evCorpGrumble, motiveNone, voiceBrash,
		"You're lucky I like you, {head}.",
	)
	addLines(evCorpGrumble, motiveNone, voiceCorporate,
		"This is outside my contracted hours, {head}. Going anyway.",
	)

	addLines(evCorpHelp, motiveNone, voiceAny,
		"Anyone in {system}? {target} won't leave me alone.",
		"{system}, {where}. {target}. I could use a hand.",
		"Getting shot in {system}. {target}, {ship}.",
	)
	addLines(evCorpHelp, motiveNone, voiceZealot,
		"Brothers, {system}. {target} assails me.",
	)
	addLines(evCorpHelp, motiveNone, voiceTribal,
		"Clan, to me! {system}, {target}.",
	)
	addLines(evCorpHelpYes, motiveNone, voiceAny,
		"Already undocked. Coming to {system}.",
		"Hang on, {target}. We see them.",
	)
	addLines(evCorpHelpNo, motiveNone, voiceAny,
		"Can't help you there, {target}. Get safe.",
		"Not enough ships to send, {target}. Warp off.",
		"{system}'s too far, {target}. Sorry.",
	)

	addLines(evCorpConsole, motiveNone, voiceAny,
		"Ouch, {target}.",
		"Glad you're okay, {target}.",
		"Want a fleet to go back for them, {target}?",
	)
	addLines(evCorpConsole, motiveNone, voiceZealot,
		"God keeps you, {target}. The ship was only a ship.",
	)
	addLines(evCorpConsole, motiveNone, voiceTribal,
		"We'll avenge it, {target}.",
	)

	addLines(evCorpClaim, motiveNone, voiceAny,
		"Claiming the {item} order. {work}.",
	)
	addLines(evCorpClaim, workMotive(workKindBuild), voiceAny,
		"On the {item} run. {n} to go.",
	)
	addLines(evCorpClaimOK, motiveNone, voiceAny,
		"Appreciated, {member}.",
		"{member}, deliver to the usual hangar.",
	)
}
