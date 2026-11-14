package rolebots

// What a bot says back to a player who answered it in Local (chatter_retort.go). One cell
// per intent; the motive is why the bot barked at them in the first place, so "why" is
// answered with the truth. {target} is the player, {race} {slur} as ever, {isk} a live
// ransom's price.

func init() {
	why := answerEvent(intentWhy)
	addLines(why, motiveNone, voiceAny,
		"Why not, {target}?",
		"Because I can, {target}.",
		"You were there, {target}. That's all.",
		"Does it matter, {target}?",
		"Ask your insurer, {target}.",
	)
	addLines(why, motiveRace, voiceAny,
		"Because you're {race}, {target}. That's reason enough.",
		"Look in a mirror, {target}. You're {race}.",
		"Because you're a {slur}, {target}.",
		"{race} don't belong out here, {target}. Simple.",
		"Your blood, {target}. Nothing you can fix.",
		"You're {race}. I don't need another reason, {slur}.",
	)
	addLines(why, motiveRace, voiceZealot,
		"Because the Scriptures say what {race} are for, {target}. It isn't flying.",
		"Because God made you to kneel, {slur}.",
	)
	addLines(why, motiveRace, voiceTribal,
		"Because of what {race} did to my people, {target}. Every one of you owes.",
		"Ask your ancestors, {slur}. They know.",
	)
	addLines(why, motiveRace, voiceCorporate,
		"{target}, {race} are a statistical risk. Risks are managed.",
	)
	addLines(why, motiveRace, voiceShowman,
		"Because a {slur} is the best audience, {target}.",
	)
	addLines(why, motiveBloodline, voiceAny,
		"Because you're {bloodline}, {target}. Not real {myrace}.",
		"{target}, your blood is the wrong blood.",
	)
	addLines(why, motiveWar, voiceAny,
		"Read your corp mail, {target}. We're at war.",
		"Your CEO knows why, {target}. Ask them.",
		"War dec, {target}. You're a target until your corp pays up or folds.",
	)
	addLines(why, motiveCorpRival, voiceAny,
		"Your corp knows what it did, {target}.",
		"Take it up with your directors, {target}.",
	)
	addLines(why, motiveFaction, voiceAny,
		"You fly for the wrong side, {target}.",
		"Your flag, {target}. Change it and we'll talk.",
	)
	addLines(why, motiveCriminal, voiceAny,
		"You're flagged, {target}. Check your overview.",
		"You broke the law, {target}. Somebody has to enforce it.",
	)
	addLines(why, motiveOutlaw, voiceAny,
		"Your sec status, {target}. Everyone can see it.",
		"You're an outlaw, {target}. You did that, not me.",
	)
	addLines(why, motiveBounty, voiceAny,
		"There's ISK on your head, {target}.",
		"Someone paid good money for you, {target}.",
	)
	addLines(why, motivePiracy, voiceAny,
		"Your cargo, {target}. Or {isk}.",
		"Toll road, {target}. {isk}.",
		"Because you're carrying something I want, {target}.",
	)
	addLines(why, motiveDefense, voiceAny,
		"You shot first, {target}.",
		"Check your logs, {target}. You started it.",
	)
	addLines(why, motivePersonal, voiceAny,
		"You know what you said, {target}.",
		"You talked too much, {target}.",
	)
	addLines(why, motiveNerve, voiceAny,
		"Because it's low-sec, {target}.",
		"I felt like it, {target}.",
		"Wrong place, wrong time, {target}.",
	)

	raceInsult := answerEvent(intentRaceInsult)
	addLines(raceInsult, motiveNone, voiceAny,
		"Say that again, {target}. I dare you.",
		"Oh, you'll pay for that one, {target}.",
		"{target}, my people have heard worse from better.",
		"Big mouth on a {race}. {target}, I'll remember you.",
		"Call me that to my face, {target}. Undock.",
		"Filthy {slur}. You'll regret that, {target}.",
		"{target}, you {slur}. I'll be seeing you.",
	)
	addLines(raceInsult, motiveNone, voiceZealot,
		"Blasphemy. {target}, God has heard you. So have I.",
		"{target}, you insult the chosen. Pray we don't meet again, {slur}.",
	)
	addLines(raceInsult, motiveNone, voiceTribal,
		"{target}, the tribes have long memories. Yours is now in it.",
		"Talk like that got your kind thrown out of Matar, {slur}.",
	)
	addLines(raceInsult, motiveNone, voiceCorporate,
		"{target}, that remark has been logged. With interest.",
	)
	addLines(raceInsult, motiveNone, voiceShowman,
		"Ooh, {target}, that's going in my memoirs. Your chapter is short.",
	)

	insult := answerEvent(intentInsult)
	addLines(insult, motiveNone, voiceAny,
		"Says the pilot in a {ship}, {target}.",
		"{target}, cute.",
		"I've killed better than you this week, {target}.",
		"Noted, {target}. You're on my list now.",
		"Keep typing, {target}. I'm keeping score.",
		"{target}, I'd be insulted if I cared.",
		"Real tough from the station, {target}.",
		"Words, {target}. I deal in killmails.",
	)
	addLines(insult, motiveNone, voiceTerse,
		"k.",
		"Noted.",
	)
	addLines(insult, motiveNone, voiceBrash,
		"LOL. {target}, my mother flies better than you, and she's a clone.",
		"Wow, {target}. Did you think of that yourself?",
	)
	addLines(insult, motiveRace, voiceAny,
		"Cry harder, {slur}.",
		"{target}, the {race} always get mouthy when they're losing.",
	)

	threat := answerEvent(intentThreat)
	addLines(threat, motiveNone, voiceAny,
		"I'll be right here, {target}.",
		"Bring friends, {target}. You'll need them.",
		"{target}, I'm not hard to find.",
		"Looking forward to it, {target}.",
		"Sure, {target}. Undock first.",
		"You and what fleet, {target}?",
	)
	addLines(threat, motiveNone, voiceCold,
		"Noted, {target}.",
		"Try, {target}.",
	)
	addLines(threat, motiveNone, voiceZealot,
		"{target}, God protects His own. Come and see.",
	)
	addLines(threat, motiveRace, voiceAny,
		"A {race} threatening me. {target}, that's adorable.",
	)

	plea := answerEvent(intentPlea)
	addLines(plea, motiveNone, voiceAny,
		"Too late, {target}.",
		"Should've thought of that before {where}, {target}.",
		"Nothing personal, {target}.",
		"{target}, begging doesn't stop guns.",
		"Hold still and it's quicker, {target}.",
	)
	addLines(plea, motivePiracy, voiceAny,
		"{isk}, {target}. Then I stop.",
		"You know how this ends, {target}. {isk}.",
		"{target}, pay {isk} and I'll be a gentleman.",
	)
	addLines(plea, motiveRace, voiceAny,
		"Your kind never begged nicely, {target}.",
		"Begging suits a {slur}, {target}.",
	)
	addLines(plea, motiveNone, voiceZealot,
		"Mercy is God's, {target}. I'm only His instrument.",
	)

	ransom := answerEvent(intentRansom)
	addLines(ransom, motiveNone, voiceAny,
		"No deals, {target}.",
		"Your ship isn't for sale, {target}. It's mine.",
		"Not interested in your ISK, {target}.",
	)
	addLines(ransom, motivePiracy, voiceAny,
		"{isk}, {target}. Same as I said.",
		"{isk}. The clock is running, {target}.",
		"Send {isk} to me and you fly away, {target}.",
		"{target}, {isk}. Not a credit less.",
	)

	gf := answerEvent(intentGF)
	addLines(gf, motiveNone, voiceAny,
		"gf {target}.",
		"o7 {target}.",
		"o7. No hard feelings, {target}.",
		"Respect, {target}.",
	)
	addLines(gf, motiveRace, voiceAny,
		"Don't o7 me, {slur}.",
		"Save it, {target}. You're still {race}.",
	)

	laugh := answerEvent(intentLaugh)
	addLines(laugh, motiveNone, voiceAny,
		"Laugh it up, {target}.",
		"Glad you're enjoying it, {target}.",
		"{target}, you won't laugh for long.",
		"Something funny, {target}?",
	)

	other := answerEvent(intentOther)
	addLines(other, motiveNone, voiceAny,
		"Whatever, {target}.",
		"Talk all you like, {target}.",
		"{target}, nobody asked.",
		"Mm-hm. {target}, sure.",
	)
	addLines(other, motiveRace, voiceAny,
		"I don't speak {race}, {target}.",
		"Nobody listens to a {slur}, {target}.",
	)
	addLines(other, motivePiracy, voiceAny,
		"Less typing, more paying. {isk}, {target}.",
	)
}

// More answers, mostly in the voices.
func init() {
	addLines(answerEvent(intentWhy), motiveNone, voiceZealot,
		"Because God willed it, {target}.",
	)
	addLines(answerEvent(intentWhy), motiveNone, voiceCorporate,
		"It made sense on a spreadsheet, {target}.",
	)
	addLines(answerEvent(intentWhy), motiveNone, voiceBrash,
		"Because it's funny, {target}.",
	)
	addLines(answerEvent(intentWhy), motiveNone, voiceTerse,
		"Because.",
	)
	addLines(answerEvent(intentInsult), motiveNone, voiceZealot,
		"{target}, your soul is louder than your guns.",
	)
	addLines(answerEvent(intentInsult), motiveNone, voiceCorporate,
		"{target}, your remark has been forwarded to my indifference department.",
	)
	addLines(answerEvent(intentInsult), motiveNone, voiceTribal,
		"{target}, the clan laughs at you.",
	)
	addLines(answerEvent(intentInsult), motiveNone, voiceShowman,
		"{target}, darling, you need better writers.",
	)
	addLines(answerEvent(intentThreat), motiveNone, voiceBrash,
		"Ooh, scary, {target}. I'm shaking.",
	)
	addLines(answerEvent(intentThreat), motiveNone, voiceTribal,
		"{target}, the clan will be waiting.",
	)
	addLines(answerEvent(intentThreat), motiveNone, voiceCorporate,
		"{target}, threats are not a business plan.",
	)
	addLines(answerEvent(intentPlea), motiveNone, voiceBrash,
		"Beg harder, {target}.",
	)
	addLines(answerEvent(intentPlea), motiveNone, voiceCold,
		"No.",
	)
	addLines(answerEvent(intentGF), motiveNone, voiceBrash,
		"gf? That wasn't a fight, {target}.",
	)
	addLines(answerEvent(intentGF), motiveNone, voiceZealot,
		"Go with God, {target}.",
	)
	addLines(answerEvent(intentLaugh), motiveNone, voiceBrash,
		"Ha. Ha. {target}, hilarious.",
	)
	addLines(answerEvent(intentOther), motiveNone, voiceTerse,
		"k.",
	)
	addLines(answerEvent(intentOther), motiveNone, voiceZealot,
		"{target}, I'll pray for you.",
	)
}
