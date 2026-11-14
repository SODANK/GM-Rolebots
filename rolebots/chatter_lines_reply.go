package rolebots

// The rest of an engagement in Local: the victim's answer, the aggressor's comeback, a
// bystander's aside, and the lines over the wreck. In a victim's line {target} is the
// attacker (slotsFor always addresses the party being spoken to), {race} is the attacker's
// people, and {slur} is what a xenophobe victim calls them. The motive is the one the victim
// was accused of, so a pilot told it is being shot for being Minmatar answers that.

func init() {
	// ── the victim answers ────────────────────────────────────────────────
	addLines(evVictim, motiveNone, voiceAny,
		"You'll have to work for it, {target}.",
		"{target}, really? Here?",
		"Come on then, {target}.",
		"I'm not dying in {where} today, {target}.",
		"{target}, you've made a mistake.",
		"Just my luck. {target}, of course.",
		"Anyone near {where}? {target} is on me.",
		"{target}, I have friends.",
		"Is that all you've got, {target}?",
	)
	addLines(evVictim, motiveNone, voiceTerse,
		"No.",
		"Try it.",
		"Hm.",
	)
	addLines(evVictim, motiveNone, voiceBrash,
		"{target}, you picked the wrong pilot, pal.",
		"Oh, it's ON, {target}.",
	)
	addLines(evVictim, motiveNone, voiceCorporate,
		"{target}, I'll be filing a loss report with your name on it.",
		"{target}, my insurer will hear about you.",
	)
	addLines(evVictim, motiveNone, voiceZealot,
		"God is my shield, {target}.",
		"{target}, I forgive you. My guns don't.",
	)
	addLines(evVictim, motiveNone, voiceShowman,
		"{target}, you've just made this interesting.",
	)
	addLines(evVictim, motiveNone, voiceTribal,
		"{target}, the tribes don't run.",
	)
	addLines(evVictim, motiveNone, voicePirate,
		"{target}, you're robbing a robber. Bold.",
	)

	// Accused of being the wrong race.
	addLines(evVictim, motiveRace, voiceAny,
		"Big words from a {race}, {target}.",
		"{target}, my people outlived yours once. We'll do it again.",
		"Of course it's a {race} bigot. {target}, come here.",
		"{target}, I've heard that before. They're all dead.",
		"Bigots die like anyone else, {target}.",
		"That's it? My blood? {target}, you're pathetic.",
		"{target}, you {slur}. Let's see who's left.",
		"Filthy {slur}. {target}, I've waited for one like you.",
		"{target}, at least my people know how to fly.",
	)
	addLines(evVictim, motiveRace, voiceTribal,
		"{target}, we broke the chains once, {slur}. We'll break you.",
		"You call me slave, {target}? Come and collect.",
		"{target}, every Holder who said that is dust now.",
	)
	addLines(evVictim, motiveRace, voiceZealot,
		"{target}, God sees your heresy, {slur}.",
		"{target}, the Throne will remember this insult.",
	)
	addLines(evVictim, motiveRace, voiceShowman,
		"{target}, in the Federation we call that a hate crime. Out here I call it a killmail.",
		"Tolerance has limits, {target}. You just found mine.",
	)
	addLines(evVictim, motiveRace, voiceCorporate,
		"{target}, prejudice is inefficient. Like your fit.",
		"{target}, the State thanks you for identifying yourself, {slur}.",
	)
	addLines(evVictim, motiveBloodline, voiceAny,
		"{target}, we're the same people, you idiot.",
		"{target}, my blood is as good as yours. Better aim, too.",
		"You'd split your own race over a bloodline, {target}? Pathetic.",
	)

	// Accused of something else.
	addLines(evVictim, motiveWar, voiceAny,
		"{target}, your war's going to cost you more than me.",
		"{target}, tell your CEO we're not surrendering.",
		"War's a two-way street, {target}.",
		"{target}, wardec warrior. Figures.",
		"{target}, we'll win this war on attrition. Starting with you.",
		"{target}, I'm not even a combat pilot. Some war.",
		"{target}, my corp's undocking. Hope you brought friends.",
		"{target}, we'll be paying you back[ in kind| with interest].",
	)
	addLines(evVictim, motiveCorpRival, voiceAny,
		"{target}, my corp will hear about this.",
		"{target}, you just restarted something.",
	)
	addLines(evVictim, motiveFaction, voiceAny,
		"{target}, politics don't fly ships. Pilots do.",
		"For my flag, {target}.",
	)
	addLines(evVictim, motiveCriminal, voiceAny,
		"{target}, I had my reasons.",
		"Yeah, yeah, I'm flagged. {target}, come get me.",
		"{target}, hero of the hour? Let's see.",
		"{target}, CONCORD's already on its way for me. You're optional.",
		"{target}, take a number. Everyone wants a piece.",
		"{target}, I've got fifteen minutes and nothing to lose.",
		"Playing cop, {target}? [You're no CONCORD.|Where's your badge?]",
		"{target}, I know the flag's up. I can read too.",
	)
	addLines(evVictim, motiveOutlaw, voiceAny,
		"{target}, you think the sec status scares me?",
		"Outlaw and proud, {target}. Come on.",
		"{target}, I was minus five before you undocked.",
		"{target}, I earned every point of that minus five.",
		"Sec status is just a number, {target}.",
		"{target}, I don't need high-sec. Do you?",
		"{target}, you want my head? [Come take it.|Get in line.]",
		"{target}, every bounty hunter says that. Once.",
	)
	addLines(evVictim, motiveBounty, voiceAny,
		"{target}, bounty hunter. Of course.",
		"{target}, whoever paid you overpaid.",
		"{target}, collect it if you can.",
		"{target}, the bounty pays on the pod. Good luck catching it.",
		"{target}, how much am I worth these days?",
		"{target}, I'll double whatever they're paying you. [Kidding.|Not really.]",
		"{target}, enjoy the ship. The ISK's in my pod.",
		"Go spend somebody else's ISK, {target}.",
	)
	addLines(evVictim, motiveDefense, voiceAny,
		"{target}, fine, you win the argument. Not the fight.",
		"{target}, that was a warning shot.",
	)
	addLines(evVictim, motivePersonal, voiceAny,
		"{target}, still upset? Really?",
		"{target}, let it go.",
	)

	// ── the aggressor's comeback ──────────────────────────────────────────
	addLines(evComeback, motiveNone, voiceAny,
		"Keep talking, {target}.",
		"{target}, we'll see.",
		"Sure, {target}. Sure.",
		"Shields first, {target}. Then the talking.",
		"{target}, your friends aren't coming.",
		"Brave words for a {ship}, {target}.",
		"Talk is cheap, {target}. Your {hull} isn't.",
		"{target}, I've heard better from pods.",
		"[Type|Talk] faster, {target}. Your {ship} hasn't got long.",
	)
	addLines(evComeback, motiveRace, voiceAny,
		"Cry about it, {slur}.",
		"{target}, the {race} always talk most right before.",
		"You're still a {race}, {target}. Nothing you say changes that.",
		"{target}, scream all you want. Nobody listens to {race}.",
	)
	addLines(evComeback, motiveRace, voiceZealot,
		"Repent, {slur}. You have about thirty seconds.",
	)
	addLines(evComeback, motiveRace, voiceTribal,
		"{target}, my people heard that for six hundred years. Your turn to listen.",
	)
	addLines(evComeback, motiveWar, voiceAny,
		"{target}, your CEO should've paid us off.",
		"{target}, take it up with your directors.",
		"{target}, we'll accept your surrender any time. Ask your CEO.",
		"War's war, {target}. Cry in corp chat.",
		"{target}, should've stayed docked [this week|until the dec ran out].",
		"{target}, your corp's killboard disagrees.",
		"{target}, every word buys another week of war.",
		"{target}, your corpmates logged off an hour ago.",
	)
	addLines(evComeback, motiveBounty, voiceAny,
		"{target}, the ISK's already spent.",
		"{target}, the contract doesn't care what you think.",
		"Argue with whoever paid, {target}. I just collect.",
		"{target}, you're worth the same whether you talk or not.",
		"{target}, save it. I've still got a pod to catch.",
		"Bounty boards don't take appeals, {target}.",
		"{target}, take it up with whoever put the ISK on you.",
		"{target}, [quiet|hush] now. You're bad for business.",
	)
	addLines(evComeback, motiveNone, voiceTerse,
		"Mm.",
		"Sure.",
	)

	// ── somebody hit a bot and the aggressor said nothing ─────────────────
	addLines(evUnderFire, motiveNone, voiceAny,
		"{target}?! What did I do?",
		"Taking fire at {where}. {target}, seriously?",
		"Hey! {target}, knock it off.",
		"{target}, you want to explain that?",
		"Who's {target} and why are they shooting me?",
		"{target}, I wasn't bothering anyone.",
		"Anyone else seeing {target} at {where}?",
		"Ow. {target}, that's rude.",
	)
	addLines(evUnderFire, motiveNone, voiceTerse,
		"Rude.",
		"{target}. Why.",
	)
	addLines(evUnderFire, motiveNone, voiceCorporate,
		"{target}, unprovoked aggression has been logged.",
	)
	addLines(evUnderFire, motiveNone, voiceZealot,
		"{target}, you strike a servant of God. Unwise.",
	)
	addLines(evUnderFire, motiveRace, voiceAny,
		"Of course it's a {race}. {target}, you {slur}.",
		"{target}, typical {race} — shoot first, think never.",
		"A {slur} with a gun. {target}, how original.",
	)

	// ── somebody else in Local remarks on it ──────────────────────────────
	// {attacker} and {victim} are both links; {target} is the victim too.
	addLines(evBystander, motiveNone, voiceAny,
		"Fight at {where}. {attacker} on {victim}.",
		"Heads up — {attacker} is shooting {victim} at {where}.",
		"Someone's getting popped at {where}.",
		"{attacker} just jumped {victim}. Avoid {where}.",
		"Saw that, {attacker}. Nice.",
		"Rip {victim}.",
		"Popcorn. {attacker} vs {victim}.",
		"{victim}, warp out, warp out.",
		"Stay off {where}, people.",
		"There goes {victim}'s {ship}.",
		"Hostile at {where}: {attacker}.",
	)
	addLines(evBystander, motiveRace, voiceAny,
		"{attacker} hunting {race} again. Charming.",
		"Wow, {attacker}. Racist much?",
		"{attacker} really hates {race}, huh.",
		"Some of us just want to mine, {attacker}.",
		"{attacker}, grow up.",
	)
	addLines(evBystander, motiveWar, voiceAny,
		"War targets fighting at {where}. Stay clear.",
		"{attacker}'s war is spilling into {where}.",
		"War dec fight at {where}. Not our problem.",
		"{attacker} hunting war targets[| again].",
		"Don't undock near {where}. Somebody's war is on.",
		"{attacker}, take your war somewhere else.",
		"Glad I'm not in that war. Good luck at {where}.",
		"Another wardec scrap at {where}. Mind the wrecks.",
	)
	addLines(evBystander, motiveCriminal, voiceAny,
		"{victim} had it coming.",
		"Justice at {where}.",
		"Should've kept it legal, {victim}.",
		"CONCORD's late again. Somebody beat them to it at {where}.",
		"{victim}, crime doesn't pay. Clearly.",
		"Flagged pilot going down at {where}. [Popcorn.|Nice.|About time.]",
		"Shoot people in public, {victim}, and the public shoots back.",
		"{victim} flashed red at {where} and found out.",
	)
	addLines(evBystander, motiveOutlaw, voiceAny,
		"Good riddance, {victim}.",
		"One less outlaw. Thanks, {attacker}.",
		"Someone finally caught {victim}.",
		"{attacker}, doing the work CONCORD won't.",
		"{victim}'s record caught up with them.",
		"Nice one, {attacker}. That one camped me last week.",
		"Red down. [Thanks, {attacker}.|o7 {attacker}.]",
		"{victim}, that's what minus five buys you.",
	)
	addLines(evBystander, motiveBounty, voiceAny,
		"Bounty hunter at {where}. {victim}, run.",
		"Somebody's collecting on {victim} at {where}.",
		"Someone wanted {victim} dead. Looks like they paid for it.",
		"{victim}, who did you upset?",
		"{victim}'s pod is worth more than the ship[| right now].",
		"Payday at {where} for somebody.",
		"Bounty hunt at {where}. Popcorn time.",
		"That price on {victim} finally got somebody's attention.",
	)
	addLines(evBystander, motiveNone, voiceTerse,
		"o7 {victim}.",
		"lol.",
	)

	// ── over the wreck ────────────────────────────────────────────────────
	// The killer: {target} is the dead pilot, {ship} the hull it lost.
	addLines(evGloat, motiveNone, voiceAny,
		"gf {target}.",
		"Thanks for the {ship}, {target}.",
		"{target}, better luck next clone.",
		"Another one. Rest well, {target}.",
		"{target}, see you at the station.",
		"Nice try, {target}.",
		"{target}, insure next time.",
	)
	addLines(evGloat, motiveNone, voiceBrash,
		"GET REKT, {target}.",
		"{target}, that {ship} folded like paper. Lol.",
		"Too easy. {target}, too easy.",
	)
	addLines(evGloat, motiveNone, voiceTerse,
		"gf.",
		"Next.",
	)
	addLines(evGloat, motiveNone, voiceCold,
		"{target}. Done.",
		"Target destroyed.",
	)
	addLines(evGloat, motiveNone, voiceZealot,
		"Go with God, {target}. Or don't.",
		"Amarr victor.",
	)
	addLines(evGloat, motiveNone, voiceCorporate,
		"Transaction complete, {target}.",
		"{target}, your loss is our quarterly gain.",
	)
	addLines(evGloat, motiveNone, voiceShowman,
		"And scene. Thank you, {target}.",
	)
	addLines(evGloat, motiveNone, voiceTribal,
		"For the tribes. gf {target}.",
	)
	addLines(evGloat, motiveRace, voiceAny,
		"One less {race}.",
		"Back to the clone vat, {slur}.",
		"{target}, that's what {race} get out here.",
		"Tell the other {race}, {target}.",
		"Stay out of {myrace} space, {slur}.",
	)
	addLines(evGloat, motiveRace, voiceZealot,
		"Cleansed. Praise be.",
	)
	addLines(evGloat, motiveWar, voiceAny,
		"Add that to the war report, {target}.",
		"{target}, tell your directors that one's on them.",
		"{target}, one more for the killboard[|. Keep them coming].",
		"Your CEO owes you a ship, {target}.",
		"{target}, surrender terms are still open. Ask your directors.",
		"Tell your corp the war's going great, {target}. For us.",
		"{target}, that's what a war dec costs. Itemised.",
		"War's not over, {target}. Undock again soon.",
	)
	addLines(evGloat, motiveBounty, voiceAny,
		"Bounty collected. Thanks, {target}.",
		"{target}, your head paid for my [next ship|evening|ammo].",
		"Pleasure doing business with your enemies, {target}.",
		"{target}, somebody out there is very happy with me.",
		"Price on your head, {target}. Paid in full[|, plus interest].",
		"{target}, tell whoever hates you the job's done.",
		"{target}, you were worth every jump.",
		"Contract closed, {target}.",
	)
	addLines(evGloat, motiveCriminal, voiceAny,
		"Justice served, {target}.",
		"{target}, CONCORD would've been slower. You're welcome.",
		"That's what the flag is for, {target}.",
		"{target}, crime pays. Just not you.",
		"Flag's still up, {target}. Ship isn't.",
		"{target}, next time keep your guns cold[| in high-sec].",
		"Law and order, {target}. Mostly order.",
		"{target}, you shot first and lost last.",
	)
	addLines(evGloat, motiveOutlaw, voiceAny,
		"Good riddance, {target}.",
		"{target}, minus five and minus a ship.",
		"One less red on the overview[|, {target}].",
		"{target}, the belts are safer already.",
		"Nobody's going to miss that ship, {target}. Except you.",
		"{target}, go rat for a month. Then we'll talk.",
		"{target}, outlaw today, fresh clone tomorrow.",
		"Public service, {target}. [Free of charge.|No need to thank me.]",
	)

	// The dead, from the pod: {target} is the killer.
	addLines(evLament, motiveNone, voiceAny,
		"gf {target}.",
		"o7 {target}. Got me.",
		"Ugh. Fair enough, {target}.",
		"{target}, I'll be back.",
		"Well, that's the {ship} gone.",
		"{target}, that was cheap.",
		"Didn't even see you coming, {target}.",
		"Remember the name, {target}. I will.",
	)
	addLines(evLament, motiveNone, voiceTerse,
		"gf.",
		"Noted.",
	)
	// The dead, when its ledger read the fight as fair (grace_ledger.go): no grudge in it.
	addLines(evGoodFight, motiveNone, voiceAny,
		"gf {target}.",
		"gf. Well flown, {target}.",
		"o7 {target}. Fair fight.",
		"{target}, gf. I'll bring a better {ship} next time.",
		"Good fight, {target}. No hard feelings.",
	)
	addLines(evGoodFight, motiveNone, voiceTerse,
		"gf.",
		"o7.",
	)
	addLines(evGoodFight, motiveNone, voiceBrash,
		"gf {target}. Rematch when I'm refit.",
		"Alright, {target}, that one's yours. gf.",
	)
	addLines(evLament, motiveNone, voiceBrash,
		"Lucky shot, {target}. LUCKY.",
		"{target}, enjoy it while it lasts.",
	)
	addLines(evLament, motiveNone, voiceCorporate,
		"{target}, my insurance covers it. Does yours cover me?",
	)
	addLines(evLament, motiveNone, voiceZealot,
		"God tests the faithful. {target}, you are not my last test.",
	)
	addLines(evLament, motiveNone, voiceTribal,
		"The tribes will remember you, {target}.",
	)

	// The dead answering a bigot, in kind where it has a word for them.
	addLines(evSpite, motiveNone, voiceAny,
		"Proud of yourself, {target}? Shooting people for their blood?",
		"{target}, you'll die a bigot. Soon, if I have anything to say about it.",
		"Enjoy it, {target}. My people outlast yours every time.",
		"{target}, the only thing worse than your fit is your mind.",
		"You {slur}. I'll be back with friends, {target}.",
		"{target}, nobody remembers bigots. They remember who killed them.",
	)
}

// More answers and asides, mostly filling in the voices.
func init() {
	addLines(evVictim, motiveNone, voiceAny,
		"{target}, you sure about this?",
		"{target}, I've got drones and a grudge.",
		"Fine. {target}, let's dance.",
		"{target}, I was having a good day.",
		"{target}, you'll pay for the paint.",
		"You'll want a bigger ship than a {ship}, {target}.",
		"Mayday, {where}. {target} on me.",
	)
	addLines(evVictim, motiveNone, voiceCold,
		"{target}. Noted.",
		"Engaging {target}.",
		"{target}, you've misjudged this.",
	)
	addLines(evVictim, motiveNone, voiceTerse,
		"Hm. {target}.",
		"Really.",
	)
	addLines(evVictim, motiveRace, voiceAny,
		"{target}, my blood is older than your empire.",
		"{target}, hate keeps you warm in a pod, I hear.",
		"{target}, you {slur}, I'll take you with me.",
		"{target}, you'd shoot a whole people? Start with me then.",
	)
	addLines(evVictim, motiveRace, voiceBrash,
		"Oh, a racist. How original, {target}.",
		"{target}, does your mother know you talk like that?",
	)
	addLines(evVictim, motiveRace, voiceCold,
		"{target}. I'll remember your face. And your race.",
	)
	addLines(evVictim, motiveRace, voiceTerse,
		"Bigot.",
		"Try, {slur}.",
	)
	addLines(evVictim, motiveWar, voiceCorporate,
		"{target}, your war is a cost your shareholders will regret.",
	)
	addLines(evVictim, motiveWar, voiceTribal,
		"{target}, the clan doesn't kneel. War or no war.",
	)
	addLines(evVictim, motiveCriminal, voiceBrash,
		"{target}, a hero! Cute. Come get your medal.",
	)
	addLines(evVictim, motiveOutlaw, voicePirate,
		"{target}, outlaws eat bounty hunters for breakfast.",
	)
	addLines(evVictim, motiveBounty, voiceBrash,
		"{target}, my bounty's not THAT high. Overkill much?",
	)
	addLines(evVictim, motiveNerve, voiceAny,
		"{target}, I wasn't even doing anything.",
		"{target}, I'm just mining, you lunatic.",
		"{target}, go pick on someone your own size.",
		"Why me, {target}?",
		"{target}, what did I ever do to you?",
		"{target}, a whole belt of rats and you pick me?",
		"{target}, have you nothing better to do?",
		"{target}, I haven't even said anything[| yet].",
	)

	addLines(evComeback, motiveNone, voiceBrash,
		"{target}, that's cute. Do it again.",
		"Ha! {target}, you're funny for a corpse.",
	)
	addLines(evComeback, motiveNone, voiceCold,
		"Noise, {target}.",
		"{target}. Irrelevant.",
	)
	addLines(evComeback, motiveNone, voiceZealot,
		"{target}, save your breath for prayer.",
	)
	addLines(evComeback, motiveNone, voiceCorporate,
		"{target}, your objection has been noted and declined.",
	)
	addLines(evComeback, motiveNone, voiceShowman,
		"{target}, wonderful line. Shame nobody will hear it twice.",
	)
	addLines(evComeback, motiveNone, voiceTribal,
		"{target}, words don't fly ships.",
	)
	addLines(evComeback, motiveRace, voiceAny,
		"{target}, you'll still be a {race} in your next clone.",
		"Is that what passes for wit among {race}, {target}?",
	)
	addLines(evComeback, motiveNerve, voiceAny,
		"{target}, wrong place, wrong time. It happens.",
		"{target}, nothing personal. Still shooting.",
		"{target}, you were the closest. That's all.",
		"{target}, I don't need a reason. Neither do my guns.",
		"Call it bad luck, {target}[| and move on].",
		"{target}, I'm bored, you're here. Simple maths.",
		"Don't take it personally, {target}. Take it to the station.",
		"Sure, complain, {target}. Guns are still cycling.",
	)

	addLines(evUnderFire, motiveNone, voiceAny,
		"{target}, I'll remember this.",
		"Okay, {target}. Now it's personal.",
		"{target}, you want trouble? Fine.",
		"Pinned at {where} by {target}. Anybody?",
	)
	addLines(evUnderFire, motiveNone, voiceBrash,
		"{target}?! Do you know who I am?",
	)
	addLines(evUnderFire, motiveNone, voiceTribal,
		"{target}, you'll answer to the clan for that.",
	)
	addLines(evUnderFire, motiveNone, voiceShowman,
		"{target}, an ambush! How dramatic.",
	)
	addLines(evUnderFire, motiveNone, voicePirate,
		"{target}, you're shooting a pirate. Bold career move.",
	)

	addLines(evBystander, motiveNone, voiceAny,
		"Somebody's having fun at {where}.",
		"{victim} isn't going to make it.",
		"{attacker} again. Of course.",
		"Reds on grid at {where}.",
		"Docking up. {attacker} is out.",
		"Neutral {attacker} active at {where}. Careful.",
		"I'd help, {victim}, but no.",
		"Shots fired, {where}.",
		"{victim}, align out, buddy.",
	)
	addLines(evBystander, motiveNone, voiceBrash,
		"Get him, {attacker}!",
		"Ooh, fight! {attacker} vs {victim}.",
	)
	addLines(evBystander, motiveNone, voiceCorporate,
		"Note to self: {where} is a liability today.",
	)
	addLines(evBystander, motiveNone, voiceZealot,
		"God help you, {victim}.",
	)
	addLines(evBystander, motiveRace, voiceAny,
		"{attacker}, not everyone hates {race}. Some of us just mine.",
		"There's {attacker} with the race stuff again.",
		"{attacker}, give it a rest.",
		"Imagine shooting someone for being {race}. Couldn't be me.",
	)
	addLines(evBystander, motiveNerve, voiceAny,
		"{attacker} is just shooting random people now.",
		"{attacker}'s bored again. Hide your ships.",
		"{attacker} is shooting anything that moves.",
		"Watch out for {attacker}. No reason needed.",
		"{attacker}, what did that guy even do?",
		"Random gank by {attacker}. [Again.|Shocking.|Who could've guessed.]",
		"Stay aligned, people. {attacker} is out hunting.",
		"{attacker}, pick on someone who shoots back.",
	)
	addLines(evBystander, motivePiracy, voiceAny,
		"Pirate at {where}. {victim}, pay up or run.",
		"{attacker} is shaking people down at {where}.",
	)

	addLines(evGloat, motiveNone, voiceAny,
		"{target}, the wreck's lovely this time of year.",
		"Hope that {ship} was insured, {target}.",
		"{target}, it's not you. It's your fit.",
	)
	addLines(evGloat, motiveNone, voicePirate,
		"Thanks for the loot, {target}.",
		"{target}, should have paid.",
	)
	addLines(evGloat, motiveRace, voiceAny,
		"{target}, clone vat's that way, {slur}.",
		"Another {race} learns the lesson.",
	)
	addLines(evGloat, motiveRace, voiceTribal,
		"That one's for my ancestors.",
	)
	addLines(evGloat, motiveRace, voiceCorporate,
		"One {race} liability, written off.",
	)
	addLines(evGloat, motiveNerve, voiceAny,
		"{target}, you were just in the way.",
		"{target}, no reason. Just felt like it.",
		"{target}, wrong belt[|, wrong day].",
		"Sorry, {target}. Slow afternoon.",
		"{target}, you were there. That was enough.",
		"{target}, don't read into it. I don't.",
		"Thanks for the entertainment, {target}.",
		"{target}, [that's how it goes out here|happens to the best of us].",
	)
	addLines(evGloat, motiveDefense, voiceAny,
		"{target}, you started it.",
		"Shouldn't have shot first, {target}.",
	)

	addLines(evLament, motiveNone, voiceAny,
		"{target}, enjoy the killmail.",
		"Back to the hangar. Thanks, {target}.",
		"I had that fit for a week, {target}.",
		"{target}, see you around.",
	)
	addLines(evLament, motiveNone, voiceShowman,
		"{target}, a fine performance. Mine was better.",
	)
	addLines(evLament, motiveNone, voicePirate,
		"{target}, I'll be back for my ship. And yours.",
	)
	addLines(evLament, motiveNone, voiceCold,
		"{target}. Next time.",
	)
	addLines(evSpite, motiveNone, voiceAny,
		"{target}, your hate doesn't make you a better pilot. Just a luckier one.",
		"{target}, your ancestors would be ashamed. Or proud. Which is worse.",
		"{target}, my people will outlive your prejudice.",
	)
}
