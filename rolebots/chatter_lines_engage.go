package rolebots

// The aggressor's line: what a bot says in Local as its point lands. Slots (slotsFor in
// chatter_voice.go): {target} the victim's name as a link, {name} plain, {race} {bloodline}
// theirs, {myrace} the speaker's, {slur} what a xenophobe calls them (absent below the
// own-race stage or with the race switch off, so a line using it is not drawn), {ship} their
// hull class, {hull} its type name, {where} the nearest celestial. Lines are short: six
// words and a name.

func init() {
	// ── race motive ──
	addLines(evEngage, motiveRace, voiceAny,
		"No {race} flies through here, {target}.",
		"{target}. Wrong people, wrong system.",
		"I can smell a {race} from across the grid, {target}.",
		"{target}, your kind isn't welcome in {myrace} space.",
		"Another {race}. Hold still, {target}.",
		"{target} — {race}. That's all the reason I need.",
		"{target}, go back where {race} belong.",
		"Out here we clean up {race} like you, {target}.",
		"{slur}. Yes, you, {target}.",
		"{target}, you {slur}. Point's on.",
		"Look at this — a {slur} in a {ship}. {target}, say goodbye.",
		"{target}, filthy {slur}. Not in my system.",
		"Get out of my sky, {slur}. {target}, I mean you.",
		"{target}. {slur}. Done talking.",
		"Every {slur} I pop is one less, {target}.",
		"Should've stayed home, {slur}. {target}, this is your stop.",
		"{target}, nobody will miss one more {slur}.",
		"A {race} in a {ship}. Bold, {target}. Stupid, but bold.",
	)
	addLines(evEngage, motiveRace, voiceZealot,
		"{target}, the Scriptures have a word for {race}. It isn't a kind one.",
		"By the light of the Throne, {target}, you are cleansed today.",
		"{target}. The Reclaiming did not end, {slur}. It came for you.",
		"Kneel, {slur}. {target}, your soul may yet be saved. Your ship will not.",
		"God's will, {target}. Your kind were made to serve, not to fly.",
		"{target}, a {slur} at the helm is a sin against the Order.",
		"The Empire remembers you, {target}. Back to the chains, {slur}.",
		"Heathen {race}. {target}, I'll pray for you after.",
		"{target}, Amarr victor. Always. Let me show you.",
	)
	addLines(evEngage, motiveRace, voiceCorporate,
		"{target}, {race} are a liability. Liabilities are written off.",
		"Nothing personal, {target}. {race} simply don't fit our projections.",
		"{target}, consider this a hostile restructuring, {slur}.",
		"The State has a policy on {race}, {target}. I'm the policy.",
		"{target}, efficiency demands I remove you. And I'm very efficient.",
		"Your contract is terminated, {slur}. {target}, clear the grid.",
		"{target}. One less {race} — the quarterly figures thank you.",
	)
	addLines(evEngage, motiveRace, voiceShowman,
		"Ladies and gentlemen — a {race}! {target}, take a bow before you go.",
		"{target}, the Federation sends its regards, {slur}.",
		"Liberty for all, {target}. Just not for {race}.",
		"Smile for the holoreels, {slur}. {target}, this is your big scene.",
		"{target}, I've waited all week for a {race}. Don't disappoint me.",
		"Encore! Another {slur} on my overview. {target}, curtain's up.",
	)
	addLines(evEngage, motiveRace, voiceTribal,
		"{target}. My grandmother wore your chains, {slur}.",
		"For the tribes, {target}. For every one your kind took.",
		"{target}, {race} blood pays for {race} sins.",
		"The Rebellion isn't over, {slur}. {target}, it's right here.",
		"I remember, {target}. Every Minmatar remembers. Burn, {slur}.",
		"{target}, you {slur}. Sanmatar's peace doesn't cover you.",
		"Freedom has a price, {target}. Today {race} pay it.",
	)
	addLines(evEngage, motiveRace, voiceTerse,
		"{race}. Dead. {target}.",
		"{slur}.",
		"Not here, {target}.",
		"{target}. No {race}.",
	)
	addLines(evEngage, motiveRace, voiceCold,
		"{target}. Your people chose the wrong side. So did you.",
		"Nothing to discuss, {target}. You're {race}.",
		"{target}, I don't share space with {race}.",
		"{target}, you {slur}. This will be quick.",
	)
	addLines(evEngage, motiveRace, voiceBrash,
		"HA. A {slur}! {target}, you just made my day.",
		"{target} the {slur}, everybody! Watch this.",
		"Oh, a {race} in a {ship}. {target}, this is going to be fun.",
		"Who let the {slur} out? {target}, back in the box.",
	)

	// ── bloodline: the final stage, the wrong blood of one's own people ────
	addLines(evEngage, motiveBloodline, voiceAny,
		"{target}, you're {bloodline}. That's not {myrace}, whatever your papers say.",
		"A {bloodline} calling itself {myrace}. {target}, I'll correct the record.",
		"{target}, the {myrace} don't need {bloodline} in their ranks.",
		"{slur}. {target}, you shame the {myrace} name.",
		"{target}, you {slur}. Same race, wrong blood.",
		"Pure {mybloodline} doesn't fly beside a {bloodline}, {target}.",
		"{target}, {bloodline} — close enough to fool outsiders. Not me.",
	)
	addLines(evEngage, motiveBloodline, voiceZealot,
		"{target}, the Throne tolerates {bloodline}. I do not.",
		"True blood answers to God, {target}. {bloodline} answer to me.",
	)
	addLines(evEngage, motiveBloodline, voiceTribal,
		"{target}, a {bloodline} — my tribe never forgot what yours did.",
		"Tribe before nation, {target}. Yours isn't mine.",
	)
	addLines(evEngage, motiveBloodline, voiceCorporate,
		"{target}, {bloodline} stock. Below specification.",
	)

	// ── war ───────────────────────────────────────────────────────────────
	addLines(evEngage, motiveWar, voiceAny,
		"{target}. You know what a war dec means, right?",
		"{target}, your corp started this. I'm finishing it.",
		"War's on, {target}. Nowhere to hide.",
		"{target} — war target, {where}. Engaging.",
		"Your directors signed your death warrant, {target}.",
		"{target}, nothing personal. It's in the war report.",
		"Found one. {target}, the war just came to {where}.",
		"{target}, CONCORD isn't coming. You're at war.",
		"Should have read the war mail, {target}.",
		"{target}. Ship's a write-off. Tell your CEO.",
	)
	addLines(evEngage, motiveWar, voiceCorporate,
		"{target}, a war is a cost centre. You're the cost.",
		"{target}, your corporation's losses are about to go up.",
	)
	addLines(evEngage, motiveWar, voiceCold,
		"{target}. Hostile. Engaging.",
		"{target}, at war is at war. No exceptions.",
	)
	addLines(evEngage, motiveWar, voiceBrash,
		"War target in a {ship}! {target}, did you forget we're at war?",
		"{target}, your corp's paying for this one. With you.",
	)
	addLines(evEngage, motiveWar, voiceZealot,
		"{target}, this war is holy. You are not.",
	)
	addLines(evEngage, motiveWar, voiceTribal,
		"{target}, you chose the fight. The tribes accept.",
	)

	// ── corporate rivalry short of war ────────────────────────────────────
	addLines(evEngage, motiveCorpRival, voiceAny,
		"{target}, your corp's been warned.",
		"Message for your CEO, {target}: stay out of our space.",
		"{target}. We know your ticker. We don't like it.",
		"{target}, my directors want your corp out of here. I'm the eviction notice.",
		"Your outfit and mine have history, {target}.",
		"{target}, consider this a reply to your corp's last stunt.",
		"I've seen your corp's name on our board, {target}. Red.",
	)
	addLines(evEngage, motiveCorpRival, voiceCorporate,
		"{target}, a hostile competitor in our market. Remedied.",
		"{target}, our board voted. You lost.",
	)
	addLines(evEngage, motiveCorpRival, voiceBrash,
		"{target}! Your corp still can't fly, I see.",
	)

	// ── faction ───────────────────────────────────────────────────────────
	addLines(evEngage, motiveFaction, voiceAny,
		"{target}, you fly for the wrong flag.",
		"{target}. Your employer and mine don't get along.",
		"Your faction's enemies are my friends, {target}.",
		"{target}, nothing personal — it's politics.",
		"Wrong colours, {target}.",
		"{target}, the people who pay me don't like the people who pay you.",
	)
	addLines(evEngage, motiveFaction, voiceZealot,
		"{target}, the Empire's enemies are God's enemies.",
	)
	addLines(evEngage, motiveFaction, voiceCorporate,
		"{target}, the State has listed your employer as hostile. Acting accordingly.",
	)
	addLines(evEngage, motiveFaction, voiceShowman,
		"{target}, for the Federation! And the cameras.",
	)
	addLines(evEngage, motiveFaction, voiceTribal,
		"{target}, you serve our enemies. That makes you one.",
	)

	// ── grudge: the pilot mouthed off in Local ────────────────────────────
	addLines(evEngage, motivePersonal, voiceAny,
		"Remember me, {target}? You had a lot to say earlier.",
		"{target}. Still feeling chatty?",
		"I don't forget a mouth like yours, {target}.",
		"{target}, you talked. Now you pay.",
		"Say it again, {target}. Go on.",
		"{target} — I told you we'd meet.",
	)

	// ── criminal: the law, more or less ───────────────────────────────────
	addLines(evEngage, motiveCriminal, voiceAny,
		"{target}, you're flagged. Fair game.",
		"Criminal on grid — {target}. Engaging.",
		"{target}, you broke the law. I'm the consequence.",
		"Should've thought before you shot, {target}.",
		"{target}, the flag's on you. So are my guns.",
		"[Criminal|Flagged pilot] {target} at {where}. Engaging.",
		"{target}, CONCORD's slow. I'm not.",
		"{target}, [CONCORD's on the way|the cops are coming]. I'm already here.",
		"Flashing red at {where}. {target}, that's you.",
		"{target}, you shot the wrong pilot in front of the wrong crowd.",
		"Fifteen minutes of fame, {target}. I'll make it shorter.",
	)
	addLines(evEngage, motiveCriminal, voiceCold,
		"{target}. Criminal flag. Weapons free.",
		"{target}, you're a target now. Your choice, not mine.",
	)
	addLines(evEngage, motiveCriminal, voiceBrash,
		"Ooh, a criminal! {target}, you're worth points.",
		"{target}, flashing red. My favourite colour.",
	)

	// ── outlaw: a record at or below -5 ───────────────────────────────────
	addLines(evEngage, motiveOutlaw, voiceAny,
		"{target}, your sec status is a public invitation.",
		"Outlaw on grid. {target}, nobody will cry for you.",
		"{target}, minus five and falling. Let me help.",
		"{target}, you're red to every pilot alive. Including me.",
		"An outlaw in a {ship}. {target}, you're everyone's business.",
		"{target}, no law protects you. You saw to that.",
		"{target}, [a record like yours|sec status that low] gets shot on sight. Hi.",
		"Every gate camp in the cluster wants you, {target}. I got here first.",
		"No CONCORD for outlaws, {target}. Just me[| and my guns].",
		"{target}, that {ship} has a minus five painted on the hull.",
		"{target}, you should've ratted more and pirated less.",
	)
	addLines(evEngage, motiveOutlaw, voiceZealot,
		"{target}, the [outlaw's|sinner's|lawless] path ends in fire. [Here's the fire.|I am the fire.|Behold it.]",
		"{target}, God keeps a ledger of the lawless. Your page is full.",
	)
	addLines(evEngage, motiveOutlaw, voiceCold,
		"{target}. [Outlaw|Red flag|Minus five]. [Clean kill|Weapons free|No questions asked].",
		"{target}, no law covers you. Neither will your armour.",
	)

	// ── bounty ────────────────────────────────────────────────────────────
	addLines(evEngage, motiveBounty, voiceAny,
		"{target}, somebody wants you dead badly enough to pay for it.",
		"Bounty's on your head, {target}. I'm collecting.",
		"{target}, nothing personal. Just business.",
		"There's a price on you, {target}. I'm here to settle it.",
		"{target}, your bounty buys my next ship.",
		"Tracked you to {where}, {target}. Long way for nothing, huh?",
		"{target}, you've got admirers. They hired me.",
	)
	addLines(evEngage, motiveBounty, voiceCorporate,
		"{target}, your bounty has matured. Collecting.",
	)
	addLines(evEngage, motiveBounty, voiceBrash,
		"{target}! Payday!",
	)

	// ── defence: they shot first ──────────────────────────────────────────
	addLines(evEngage, motiveDefense, voiceAny,
		"Bad idea, {target}.",
		"{target}, you shot first. Remember that.",
		"Oh, you want a fight, {target}? Fine.",
		"{target}, wrong ship to poke.",
		"Returning fire on {target}.",
		"You picked this, {target}.",
		"{target}, that was your one free shot.",
		"Taking fire from {target}. Answering.",
	)
	addLines(evEngage, motiveDefense, voiceTerse,
		"Mistake, {target}.",
		"Your funeral.",
		"Engaging {target}.",
	)
	addLines(evEngage, motiveDefense, voiceBrash,
		"{target}, did you really just shoot at me? Really?",
		"Ha! {target} wants to dance.",
	)
	addLines(evEngage, motiveDefense, voiceZealot,
		"{target}, you raised your hand against the faithful. God sees.",
	)
	addLines(evEngage, motiveDefense, voiceCorporate,
		"{target}, hostile action noted. Responding in kind.",
	)
	addLines(evEngage, motiveDefense, voiceShowman,
		"{target}, you started the show. I'll end it.",
	)
	addLines(evEngage, motiveDefense, voiceTribal,
		"{target}, strike a Matari and the Matari strike back.",
	)
	// Only renders where {slur} fills: a xenophobe answering fire.
	addLines(evEngage, motiveDefense, voiceAny,
		"A {slur} shooting at me? {target}, you'll regret that.",
	)

	// ── fleet call ────────────────────────────────────────────────────────
	addLines(evEngage, motiveFleetCall, voiceAny,
		"{target} is primary.",
		"FC says {target}. Locking.",
		"Primary {target}, {ship}.",
		"On {target}. Everyone on {target}.",
		"{target}, you've been called. Sorry.",
	)

	// ── nerve: a stranger, and the pilot felt like it ─────────────────────
	addLines(evEngage, motiveNerve, voiceAny,
		"{target}. Wrong place, wrong time.",
		"Nothing personal, {target}. You were just there.",
		"{target}, that's a nice {ship}. Was.",
		"Hello, {target}. Goodbye, {target}.",
		"{target}, this is low-sec. You knew that.",
		"Out here everyone's a target, {target}.",
		"{target}, I was bored. You're the cure.",
		"You flew into {where}, {target}. That's on you.",
		"{target}, don't take it personally. I don't.",
		"Just passing through, {target}? Not anymore.",
	)
	addLines(evEngage, motiveNerve, voiceBrash,
		"{target}! Fresh {ship}, fresh kill.",
		"{target}, sit still and look pretty.",
		"Somebody's having a bad day, {target}. Hint: you.",
	)
	addLines(evEngage, motiveNerve, voiceCold,
		"{target}. Target of opportunity.",
		"{target}, you'll do.",
	)
	addLines(evEngage, motiveNerve, voiceTerse,
		"{target}.",
		"Hi.",
		"Point on {target}.",
	)
	addLines(evEngage, motiveNerve, voiceShowman,
		"{target}, you're in my show now.",
	)
	addLines(evEngage, motiveNerve, voiceCorporate,
		"{target}, an acquisition opportunity.",
	)

	// ── the general pool: any motive ──────────────────────────────────────
	addLines(evEngage, motiveNone, voiceAny,
		"{target}. Hold still.",
		"Engaging {target}.",
		"{target}, this is going to hurt.",
		"Point on {target}.",
		"{target}, you're mine.",
		"{target}, [don't bother aligning|stay put]. It won't help.",
		"{target}, hope your clone's up to date.",
		"Sorry, {target}. Not sorry.",
		"{target}, warp if you can.",
		"Locked and cycling, {target}.",
	)
}

// More voices for the law-and-order and bounty cells, and for the stranger: the cells a
// combat patrol draws from most.
func init() {
	addLines(evEngage, motiveCriminal, voiceZealot,
		"{target}, the law is God's. You broke it.",
		"Sinner on grid. {target}, atone.",
	)
	addLines(evEngage, motiveCriminal, voiceCorporate,
		"{target}, you're in breach. Enforcement follows.",
		"{target}, your flag is a liability I'm authorised to close.",
	)
	addLines(evEngage, motiveCriminal, voiceShowman,
		"{target}, wanted dead or alive. I'm not picky.",
		"{target}, a live criminal on my grid! [The audience adores a hanging.|Hold for applause.]",
	)
	addLines(evEngage, motiveCriminal, voiceTribal,
		"{target}, among the tribes we hang thieves. Out here I just shoot them.",
		"{target}, the clans settle crimes quickly. So do I.",
	)
	addLines(evEngage, motiveCriminal, voiceTerse,
		"Flagged. Dead.",
		"{target}. Criminal.",
	)
	addLines(evEngage, motiveOutlaw, voiceCorporate,
		"{target}, your [record|security standing|file] has been audited. [You failed.|Findings: terminal.|Outcome: write-off.]",
		"{target}, an outlaw is an unprotected asset. Seizing.",
	)
	addLines(evEngage, motiveOutlaw, voiceShowman,
		"The famous {target}! Minus-five club. I'm a fan of your killmails — the ones you're in.",
		"{target}, the most wanted pilot on my overview! [Give the people a show.|Smile for the killmail.]",
	)
	addLines(evEngage, motiveOutlaw, voiceTribal,
		"{target}, you've no clan and no law. Just me.",
		"{target}, even the clans cast out pilots like you. I finish the job.",
	)
	addLines(evEngage, motiveOutlaw, voiceBrash,
		"[Look at|Check out|Would you look at] that sec status, {target}! You're basically a [loot piñata|walking killmail|free lunch].",
		"{target}, minus five?! [Christmas came early.|Free kill, everybody!]",
	)
	addLines(evEngage, motiveOutlaw, voiceTerse,
		"Outlaw. {target}.",
		"{target}. Free kill.",
	)
	addLines(evEngage, motiveBounty, voiceZealot,
		"{target}, the faithful pay well to see you gone.",
	)
	addLines(evEngage, motiveBounty, voiceShowman,
		"{target}, your face is on every board in the region. Hold that pose.",
	)
	addLines(evEngage, motiveBounty, voiceTribal,
		"{target}, a debt is owed. I'm the collector.",
	)
	addLines(evEngage, motiveBounty, voiceCold,
		"{target}. Contract. Nothing more.",
		"{target}, you're worth more dead.",
	)
	addLines(evEngage, motiveBounty, voiceTerse,
		"Bounty. {target}.",
	)
	addLines(evEngage, motiveNerve, voiceZealot,
		"{target}, God put you in my path. Who am I to argue?",
		"{target}, consider this a test of faith.",
	)
	addLines(evEngage, motiveNerve, voiceTribal,
		"{target}, the hunt doesn't ask permission.",
		"{target}. Good hunting — for me.",
	)
	addLines(evEngage, motiveNerve, voicePirate,
		"{target}, this is a robbery without the paperwork.",
		"Anything in that hold, {target}? We'll find out.",
	)
	addLines(evEngage, motiveNerve, voiceAny,
		"{target}, you look like you could use some excitement.",
		"{target}, you picked a lonely belt to sit in.",
		"Whoops, {target}. My finger slipped.",
		"{target}, nobody comes to {where} by accident.",
		"{target}, you've got a nice ship. I've got guns.",
		"{target}, you should've kept your overview tidier.",
	)
	addLines(evEngage, motiveWar, voiceShowman,
		"{target}, the war has a new episode. You're in it.",
	)
	addLines(evEngage, motiveWar, voiceTerse,
		"War target. {target}.",
	)
	addLines(evEngage, motiveWar, voiceAny,
		"{target}, every ship you lose is a vote to surrender.",
		"{target}, war's not over until your CEO says it is.",
		"{target}, saw your name on the war sheet.",
	)
	addLines(evEngage, motiveCorpRival, voiceAny,
		"{target}, my corp sends its regards.",
		"{target}, your corp owes us. You'll do as payment.",
		"{target}, you fly for the wrong outfit.",
	)
	addLines(evEngage, motiveCorpRival, voiceCold,
		"{target}. Your corporation. My guns.",
	)
	addLines(evEngage, motiveCorpRival, voiceZealot,
		"{target}, your corporation is an abomination before the Throne.",
	)
	addLines(evEngage, motiveFaction, voiceCold,
		"{target}. Hostile flag.",
	)
	addLines(evEngage, motiveFaction, voiceBrash,
		"{target}! Love your colours. Hate your colours.",
	)
	addLines(evEngage, motiveFaction, voiceAny,
		"{target}, the war between our employers just got smaller.",
		"{target}, it's not you. It's who you work for.",
	)
	addLines(evEngage, motivePersonal, voiceBrash,
		"{target}! Remember me? You called me names.",
	)
	addLines(evEngage, motivePersonal, voiceCold,
		"{target}. You talk. I shoot.",
	)
	addLines(evEngage, motiveDefense, voiceAny,
		"{target}, big mistake in a {ship}.",
		"{target}, you could've just flown past.",
		"{target}, my guns were cold. Not anymore.",
	)
	addLines(evEngage, motiveFleetCall, voiceAny,
		"Locking {target}. All guns.",
		"{target} on the board. Primary.",
		"Called: {target}.",
	)
	addLines(evEngage, motiveRace, voiceAny,
		"{target}, my people built this space. Yours just squat in it.",
		"{target}, I've been looking for a {race} all day.",
		"{target}, the {myrace} remember what {race} are.",
		"Huh, a {race}. {target}, lost?",
		"{target}, this sector is {myrace}. Leave or burn.",
		"Didn't anyone warn you, {target}? We don't serve {race} here.",
		"{target}, you {slur}, you're a long way from home.",
		"{target}, I'd say sorry, but you're a {slur}.",
	)
	addLines(evEngage, motiveRace, voiceZealot,
		"{target}, Amarr is the light. {race} are the dark it burns away.",
		"{target}, the faithful will sing of the {slur} I struck down today.",
	)
	addLines(evEngage, motiveRace, voiceCorporate,
		"{target}, your people's market share in this system is about to hit zero.",
		"{target}, the State does not negotiate with {slur}.",
	)
	addLines(evEngage, motiveRace, voiceShowman,
		"{target}, a {race}! The crowd goes wild.",
		"{target}, the Federation loves everyone. I'm the exception, {slur}.",
	)
	addLines(evEngage, motiveRace, voiceTribal,
		"{target}, the tribes swore an oath about {race}. I keep my oaths.",
		"{target}, Matar is free. Its sky isn't yours, {slur}.",
	)
	addLines(evEngage, motiveRace, voicePirate,
		"{target}, a {race}? I'd rob you anyway, but this is more fun.",
	)
}
