package rolebots

// Banter lines (chatter_banter.go). The topic is the opener's people; the reply's cell is
// the answering pilot's tone (curious, polite, warm, cold, scorn) and the closer answers it.
// Slots: {target} the other pilot as a link, {name} plain, {race} their people, {myrace} the
// speaker's own. {slur} appears only in a cold reply and renders only in Local, for a
// far-gone xenophobe with "Race insults" on; every cell also has lines without it. The scorn
// cell is Local's own: in an alliance channel a xenophobe is merely cold (banterTone).
// Small talk is what a pilot of that people would say over a drink in a station bar: lore,
// not quarrels, no talk of the game.

func init() {
	// ── Amarr ─────────────────────────────────────────────────────────────
	addLines(banterOpen(raceAmarr), motiveNone, voiceAny,
		"Heard the Scriptures read at the station chapel this morning. Never fails to steady me.",
		"Holder's day back home. The whole estate will be at prayer.",
		"Say what you like about the Empire, our stations are clean and quiet.",
		"I miss the gardens on Amarr Prime. Nothing out here grows like that.",
		"Spent the evening with the old texts. The Reclaiming reads differently out here.",
		"The Emperor's health was in the bulletins again. Everyone back home holds their breath.",
	)
	addLines(banterOpen(raceAmarr), motiveNone, voiceZealot,
		"God orders all things, even the belts. I find that a comfort.",
		"Did you keep the evening prayer? Out here it's easy to forget.",
	)
	addLines(banterOpen(raceAmarr), motiveNone, voiceTerse,
		"Prayers at the chapel tonight.",
	)
	addLines(banterReply(raceAmarr), motiveNone, voiceAny,
		"Each to their own.",
		"Sounds peaceful.",
		"Hm. Can't say I've ever thought about it.",
	)
	addLines(banterReply(raceAmarr), toneCurious, voiceAny,
		"What is it you read, exactly? I've only heard the Scriptures second hand.",
		"Is it true every Holder keeps their own chapel? I've always wondered.",
		"Tell me about the gardens. I've only seen Amarr Prime on the holoreels.",
	)
	addLines(banterReply(raceAmarr), tonePolite, voiceAny,
		"Can't say I share it, but I'm glad it helps.",
		"Clean stations I'll grant you.",
		"[Good|It's good] to have something to hold on to out here.",
	)
	addLines(banterReply(raceAmarr), toneWarm, voiceAny,
		"Ha. You'd like my mother, she prays enough for both of us.",
		"I'll take quiet stations any day.",
		"Ha. Say one for me while you're at it.",
	)
	addLines(banterReply(raceAmarr), toneCold, voiceAny,
		"Spare me the sermon.",
		"Your Scriptures are your business.",
		"[Good for you.|How nice for you.]",
	)
	addLines(banterReply(raceAmarr), toneScorn, voiceAny,
		"Quiet, because the slaves don't get to talk.",
		"Keep your God to yourself.",
		"Keep your God, {slur}.",
	)
	addLines(banterClose(raceAmarr), toneCurious, voiceAny,
		"Come to the chapel some time. Nobody will ask you to kneel.",
		"Happy to lend you a copy. Read the Book of Reclaiming first.",
		"Ask anything. Most outsiders never do.",
	)
	addLines(banterClose(raceAmarr), tonePolite, voiceAny,
		"Fair enough.",
		"Fly safe.",
		"Thank you. God keep you.",
	)
	addLines(banterClose(raceAmarr), toneWarm, voiceAny,
		"Your mother sounds wise.",
		"[Ha.|Heh.] You'd be welcome at our table any day.",
		"I'll say a prayer for you. A short one.",
	)
	addLines(banterClose(raceAmarr), toneCold, voiceAny,
		"I'll pray for you anyway.",
		"As you like.",
		"Then we've nothing more to say.",
	)
	addLines(banterClose(raceAmarr), toneScorn, voiceAny,
		"Mind your tongue. This is still a civilised station.",
		"I'll pray for you anyway.",
		"The Empire has outlasted louder mouths than yours.",
	)

	// ── Caldari ───────────────────────────────────────────────────────────
	addLines(banterOpen(raceCaldari), motiveNone, voiceAny,
		"Quarterly figures came in. My old megacorp is up again, if anyone cares.",
		"Back in the State nobody wastes a minute. Out here everybody wastes hours.",
		"I keep a picture of the Caldari Prime coast over my bunk. Haven't seen it in years.",
		"The Tribunal published new targets. Everyone back home is working doubles.",
		"Had real tea at the station today. Not the vat stuff. Almost wept.",
	)
	addLines(banterOpen(raceCaldari), motiveNone, voiceCorporate,
		"I've optimised my undock routine down to nine seconds. Small gains add up.",
		"Productivity in this system is a disgrace. Nobody plans.",
	)
	addLines(banterOpen(raceCaldari), motiveNone, voiceTerse,
		"Figures are good this quarter.",
	)
	addLines(banterReply(raceCaldari), motiveNone, voiceAny,
		"Good for them.",
		"Sounds like a lot of work.",
		"[Huh.|Hm.] Can't say I'd know.",
	)
	addLines(banterReply(raceCaldari), toneCurious, voiceAny,
		"Do you really work for one company your whole life? I couldn't.",
		"What's Caldari Prime like now? The Federation side never talks about it.",
		"What does a megacorp give you that a corporation out here doesn't?",
	)
	addLines(banterReply(raceCaldari), tonePolite, voiceAny,
		"Hard to argue with results.",
		"I'll take your word for it.",
		"Sounds like you've earned it.",
	)
	addLines(banterReply(raceCaldari), toneWarm, voiceAny,
		"Real tea? Where? I'll fly there right now.",
		"Nine seconds. I'm impressed and a little worried about you.",
		"Ha. You Caldari never switch off, do you? I like it.",
	)
	addLines(banterReply(raceCaldari), toneCold, voiceAny,
		"Nobody asked for your quarterly figures.",
		"Some of us have a life outside a ledger.",
		"I'll pass on the [corporate brochure|company newsletter].",
	)
	addLines(banterReply(raceCaldari), toneScorn, voiceAny,
		"A whole people that can't think without a manager.",
		"Go count something, {slur}.",
		"Wage slaves in nicer uniforms. And you still lost your own planet.",
		"Go file a report about it.",
	)
	addLines(banterClose(raceCaldari), toneCurious, voiceAny,
		"It isn't the company. It's knowing where you stand.",
		"Cold sea, grey sky, the best fish in the cluster. I'll get back one day.",
		"Ask me again over tea. Real tea.",
	)
	addLines(banterClose(raceCaldari), tonePolite, voiceAny,
		"Results are all that matter.",
		"Fly efficiently.",
		"Thank you. [Efficiency is its own reward.|Back to work, then.]",
	)
	addLines(banterClose(raceCaldari), toneWarm, voiceAny,
		"Third station from the gate. Tell them I sent you.",
		"Ha. You'd do well in the State.",
		"Next round's on me. I've budgeted for it.",
	)
	addLines(banterClose(raceCaldari), toneCold, voiceAny,
		"Suit yourself.",
		"Your loss.",
		"Fine. The figures don't care what you think.",
	)
	addLines(banterClose(raceCaldari), toneScorn, voiceAny,
		"Noted. And filed.",
		"Charming.",
		"Spoken like someone who's never met a deadline.",
	)

	// ── Gallente ──────────────────────────────────────────────────────────
	addLines(banterOpen(raceGallente), motiveNone, voiceAny,
		"There's a new holoreel out from Luminaire. Everyone back home is talking about it.",
		"Federation elections are coming. My feed's nothing but speeches.",
		"Found a station bar here that does proper Gallentean wine. Civilisation at last.",
		"The best thing about the Federation? Nobody tells you what to believe.",
		"Saw a concert relay from Caille last night. The cluster needs more music.",
	)
	addLines(banterOpen(raceGallente), motiveNone, voiceShowman,
		"You know, this hull's paint job alone is worth more than the ship. Admire it.",
		"If my life were a holoreel this would be the slow part before the explosions.",
	)
	addLines(banterOpen(raceGallente), motiveNone, voiceTerse,
		"Good wine on this station.",
	)
	addLines(banterReply(raceGallente), motiveNone, voiceAny,
		"Not my sort of thing.",
		"Enjoy it.",
		"If you say so.",
	)
	addLines(banterReply(raceGallente), toneCurious, voiceAny,
		"How do your elections even work? Everyone votes for everything?",
		"What's Luminaire like? I've never been past the border.",
		"Which bar? I've never had Gallentean wine.",
	)
	addLines(banterReply(raceGallente), tonePolite, voiceAny,
		"Freedom has its price, I hear.",
		"I'll take your word on the wine.",
		"Sounds nice, honestly.",
	)
	addLines(banterReply(raceGallente), toneWarm, voiceAny,
		"Save me a glass.",
		"Ha. The slow part is my favourite.",
		"Ha. Go on, tell me more. I've got time.",
	)
	addLines(banterReply(raceGallente), toneCold, voiceAny,
		"Freedom to argue forever. Lovely.",
		"Not interested.",
		"Spare me the culture lecture.",
	)
	addLines(banterReply(raceGallente), toneScorn, voiceAny,
		"All talk and paint, the Federation.",
		"Keep your speeches, {slur}.",
		"The Federation: loud, pretty and very pleased with itself.",
		"Nobody here cares about your [holoreels|elections].",
	)
	addLines(banterClose(raceGallente), toneCurious, voiceAny,
		"Everyone votes, everyone complains, and somehow it works.",
		"Second level, by the docking ring. Ask for the red.",
		"Come to the Federation some time. You'll see.",
	)
	addLines(banterClose(raceGallente), tonePolite, voiceAny,
		"It's not for everyone.",
		"To each their own.",
		"Thanks. Enjoy your evening.",
	)
	addLines(banterClose(raceGallente), toneWarm, voiceAny,
		"Only if you bring the cheese.",
		"Ha! A table for two at the bar, then.",
		"You're good company. Rare out here.",
	)
	addLines(banterClose(raceGallente), toneCold, voiceAny,
		"Somebody's grumpy.",
		"And yet here we both are.",
		"Free to disagree. That's rather the point.",
	)
	addLines(banterClose(raceGallente), toneScorn, voiceAny,
		"Lovely manners. Truly.",
		"And yet here we both are.",
		"Somebody needs a drink more than I do.",
	)

	// ── Minmatar ──────────────────────────────────────────────────────────
	addLines(banterOpen(raceMinmatar), motiveNone, voiceAny,
		"My clan's having its gathering this week. I'll miss it again.",
		"Got a voice-note from my grandmother. She still tells the old Rebellion stories.",
		"Rust and duct tape, they say about our ships. They still fly, don't they?",
		"Somebody in the bar was playing the old Brutor drums. Took me straight home.",
		"Every Matari out here knows somebody who never came home from the Empire.",
	)
	addLines(banterOpen(raceMinmatar), motiveNone, voiceTribal,
		"The tribes remember everything. Every debt, every friend.",
		"I wear my clan's marks under the flight suit. Always.",
	)
	addLines(banterOpen(raceMinmatar), motiveNone, voiceTerse,
		"Clan gathering this week. Missing it.",
	)
	addLines(banterReply(raceMinmatar), motiveNone, voiceAny,
		"Family matters.",
		"Sounds like a good time.",
		"Mm. I hear you.",
	)
	addLines(banterReply(raceMinmatar), toneCurious, voiceAny,
		"What happens at a gathering? I've only heard rumours.",
		"What do the marks mean? Is it your tribe or your clan?",
		"Tell me one of the Rebellion stories some time.",
	)
	addLines(banterReply(raceMinmatar), tonePolite, voiceAny,
		"I'm sorry to hear it.",
		"Your ships do fly. I'll give you that.",
		"That's worth holding on to.",
	)
	addLines(banterReply(raceMinmatar), toneWarm, voiceAny,
		"Your grandmother sounds like a legend.",
		"Rust is honest. I'll fly with you any day.",
		"Sounds like you've good people behind you.",
	)
	addLines(banterReply(raceMinmatar), toneCold, voiceAny,
		"Everyone has a sad story.",
		"Not my clan, not my business.",
		"The past is the past.",
	)
	addLines(banterReply(raceMinmatar), toneScorn, voiceAny,
		"Held together with tape, like everything else of yours.",
		"Nobody cares, {slur}.",
		"The Republic can't even keep its own ships painted.",
		"[Free, and still|Free, and yet] nobody wants to live there.",
	)
	addLines(banterClose(raceMinmatar), toneCurious, voiceAny,
		"Stories, fires and too much drink. Come to one, if your corp lets you.",
		"The clan first. The tribe is the clan's clan.",
		"Ask me again at a gathering. There's always a story.",
	)
	addLines(banterClose(raceMinmatar), tonePolite, voiceAny,
		"Thanks.",
		"We get by.",
		"Kind of you.",
	)
	addLines(banterClose(raceMinmatar), toneWarm, voiceAny,
		"She'd like you. Mostly.",
		"Ha. Fly with us some time. Bring something to drink.",
		"You'd fit in at a gathering, I think.",
	)
	addLines(banterClose(raceMinmatar), toneCold, voiceAny,
		"Suit yourself.",
		"And still we're free.",
		"Not everyone has to understand.",
	)
	addLines(banterClose(raceMinmatar), toneScorn, voiceAny,
		"We remember who said that.",
		"And still we're free.",
		"Tape holds. So do we.",
	)

	// ── anyone else: home and station life ────────────────────────────────
	addLines(banterOpen(0), motiveNone, voiceAny,
		"This station's coffee tastes like coolant.",
		"Docking ring on this station is a nightmare. Nearly scraped my hull.",
		"Anyone else miss a planet's sky? Out here it's all black.",
		"Long day. Long week. Long year.",
	)
	addLines(banterReply(0), motiveNone, voiceAny,
		"Same.",
		"Tell me about it.",
		"Isn't it always.",
	)
	addLines(banterReply(0), toneCurious, voiceAny,
		"Where are you from, originally?",
		"How long have you been out here?",
		"What would you rather be doing?",
	)
	addLines(banterReply(0), toneWarm, voiceAny,
		"Ha. Want a better cup? There's a place on the promenade.",
		"[Ha.|Heh.] Hang in there.",
		"Know the feeling. Drink's on me if you're docked.",
	)
	addLines(banterReply(0), toneCold, voiceAny,
		"Complain somewhere else.",
		"And?",
		"Everyone's tired. Get on with it.",
	)
	addLines(banterReply(0), toneScorn, voiceAny,
		"Nobody asked.",
		"Then go home.",
		"Spare us the moaning.",
	)
	addLines(banterClose(0), motiveNone, voiceAny,
		"Fair enough.",
		"o7",
		"Anyway. [Back to it.|Back to work.]",
	)
}

// banterLineTopics is every opener people a banter pool is written for, as
// race IDs (0 = anyone else), for the tests.
var banterLineTopics = []int64{raceAmarr, raceCaldari, raceGallente, raceMinmatar, 0}
